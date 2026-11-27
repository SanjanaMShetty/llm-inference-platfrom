// Package fakebackend is a deterministic, GPU-free stand-in for an
// OpenAI-compatible model server such as vLLM. It streams chat completions as
// server-sent events with a configurable first-token delay and token rate, and
// exposes queue-like Prometheus gauges so the rest of the platform can be
// developed and tested without a real model.
package fakebackend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Per-request overrides, handy for experiments and failure injection.
const (
	headerFirstTokenDelay = "X-Fake-First-Token-Delay" // Go duration, e.g. "250ms"
	headerTokenRate       = "X-Fake-Token-Rate"        // tokens per second, e.g. "100"
	headerFailAfterTokens = "X-Fake-Fail-After-Tokens" // streaming only: abort after N tokens
)

const (
	maxAllowedTokens = 8192
	maxBodyBytes     = 1 << 20
)

// Server implements http.Handler via Handler().
type Server struct {
	cfg   Config
	log   *slog.Logger
	slots chan struct{} // nil when concurrency is unlimited

	running       atomic.Int64
	waiting       atomic.Int64
	requestsTotal atomic.Int64
	tokensTotal   atomic.Int64
	disconnects   atomic.Int64
}

// New validates cfg and returns a Server. A nil logger discards logs.
func New(cfg Config, logger *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{cfg: cfg, log: logger}
	if cfg.MaxConcurrency > 0 {
		s.slots = make(chan struct{}, cfg.MaxConcurrency)
	}
	return s, nil
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": s.cfg.ModelName, "object": "model", "owned_by": "fakebackend"},
		},
	})
}

// options are the effective timing settings for one request.
type options struct {
	firstTokenDelay time.Duration
	tokensPerSecond float64
	failAfter       int
}

func (o options) interval() time.Duration {
	if o.tokensPerSecond <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / o.tokensPerSecond)
}

// callParams carries everything one request needs after validation.
type callParams struct {
	id           string
	model        string
	created      int64
	promptTokens int
	maxTokens    int
	includeUsage bool
	opts         options
}

func (p callParams) chunk(d delta, finish *string) chunk {
	return chunk{
		ID:      p.id,
		Object:  "chat.completion.chunk",
		Created: p.created,
		Model:   p.model,
		Choices: []chunkChoice{{Index: 0, Delta: d, FinishReason: finish}},
	}
}

func (s *Server) resolveOptions(r *http.Request) (options, error) {
	o := options{
		firstTokenDelay: s.cfg.FirstTokenDelay,
		tokensPerSecond: s.cfg.TokensPerSecond,
	}
	if v := r.Header.Get(headerFirstTokenDelay); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return o, fmt.Errorf("invalid %s %q: want a non-negative Go duration such as 250ms", headerFirstTokenDelay, v)
		}
		o.firstTokenDelay = d
	}
	if v := r.Header.Get(headerTokenRate); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
			return o, fmt.Errorf("invalid %s %q: want a non-negative number", headerTokenRate, v)
		}
		o.tokensPerSecond = f
	}
	if v := r.Header.Get(headerFailAfterTokens); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return o, fmt.Errorf("invalid %s %q: want a non-negative integer", headerFailAfterTokens, v)
		}
		o.failAfter = n
	}
	return o, nil
}

func (s *Server) resolveMaxTokens(req *chatRequest) (int, error) {
	n := s.cfg.DefaultMaxTokens
	switch {
	case req.MaxCompletionTokens != nil:
		n = *req.MaxCompletionTokens
	case req.MaxTokens != nil:
		n = *req.MaxTokens
	}
	if n < 1 || n > maxAllowedTokens {
		return 0, fmt.Errorf("max_tokens must be between 1 and %d", maxAllowedTokens)
	}
	return n, nil
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	// Drain the body so the server can notice a client disconnect while we stream.
	_, _ = io.Copy(io.Discard, r.Body)

	maxTokens, err := s.resolveMaxTokens(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	opts, err := s.resolveOptions(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	model := req.Model
	if model == "" {
		model = s.cfg.ModelName
	}
	p := callParams{
		id:           newID(),
		model:        model,
		created:      time.Now().Unix(),
		promptTokens: req.promptTokens(),
		maxTokens:    maxTokens,
		includeUsage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage,
		opts:         opts,
	}

	s.requestsTotal.Add(1)
	if err := s.acquire(r.Context()); err != nil {
		s.disconnects.Add(1)
		s.log.Info("client left while queued", "id", p.id)
		return
	}
	defer s.release()

	if req.Stream {
		s.serveStream(w, r, p)
		return
	}
	s.serveBlocking(w, r, p)
}

// acquire waits for a generation slot. While waiting, the request counts as
// "waiting" in the metrics. It returns an error if the client gives up first.
func (s *Server) acquire(ctx context.Context) error {
	if s.slots == nil {
		s.running.Add(1)
		return nil
	}
	s.waiting.Add(1)
	select {
	case s.slots <- struct{}{}:
		s.waiting.Add(-1)
		s.running.Add(1)
		return nil
	case <-ctx.Done():
		s.waiting.Add(-1)
		return ctx.Err()
	}
}

func (s *Server) release() {
	s.running.Add(-1)
	if s.slots != nil {
		<-s.slots
	}
}

func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, p callParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this connection")
		return
	}
	ctx := r.Context()
	start := time.Now()
	emitted := 0
	defer func() {
		s.log.Info("stream finished",
			"id", p.id,
			"tokens", emitted,
			"duration", time.Since(start).Round(time.Millisecond).String())
	}()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // ask reverse proxies not to buffer
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Simulated prefill.
	if !sleepCtx(ctx, p.opts.firstTokenDelay) {
		s.disconnects.Add(1)
		return
	}
	if err := writeEvent(w, flusher, p.chunk(delta{Role: "assistant"}, nil)); err != nil {
		s.disconnects.Add(1)
		return
	}

	// Decode loop. Token i is due at first+i*interval, so scheduling jitter
	// does not accumulate into the measured rate.
	interval := p.opts.interval()
	next := time.Now()
	for i := 0; i < p.maxTokens; i++ {
		if p.opts.failAfter > 0 && i == p.opts.failAfter {
			s.log.Warn("injecting mid-stream failure", "id", p.id, "after_tokens", i)
			panic(http.ErrAbortHandler) // closes the connection without a clean end
		}
		if i > 0 {
			next = next.Add(interval)
			if !sleepCtx(ctx, time.Until(next)) {
				s.disconnects.Add(1)
				return
			}
		}
		if err := writeEvent(w, flusher, p.chunk(delta{Content: tokenText(i)}, nil)); err != nil {
			s.disconnects.Add(1)
			return
		}
		s.tokensTotal.Add(1)
		emitted++
	}

	stop := "stop"
	if err := writeEvent(w, flusher, p.chunk(delta{}, &stop)); err != nil {
		s.disconnects.Add(1)
		return
	}
	if p.includeUsage {
		u := usage{PromptTokens: p.promptTokens, CompletionTokens: emitted, TotalTokens: p.promptTokens + emitted}
		final := chunk{
			ID:      p.id,
			Object:  "chat.completion.chunk",
			Created: p.created,
			Model:   p.model,
			Choices: []chunkChoice{},
			Usage:   &u,
		}
		if err := writeEvent(w, flusher, final); err != nil {
			s.disconnects.Add(1)
			return
		}
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		s.disconnects.Add(1)
		return
	}
	flusher.Flush()
}

func (s *Server) serveBlocking(w http.ResponseWriter, r *http.Request, p callParams) {
	total := p.opts.firstTokenDelay + time.Duration(p.maxTokens-1)*p.opts.interval()
	if !sleepCtx(r.Context(), total) {
		s.disconnects.Add(1)
		return
	}
	var content strings.Builder
	for i := 0; i < p.maxTokens; i++ {
		content.WriteString(tokenText(i))
	}
	s.tokensTotal.Add(int64(p.maxTokens))

	body := completion{
		ID:      p.id,
		Object:  "chat.completion",
		Created: p.created,
		Model:   p.model,
		Choices: []completionChoice{{
			Index:        0,
			Message:      message{Role: "assistant", Content: content.String()},
			FinishReason: "stop",
		}},
		Usage: usage{PromptTokens: p.promptTokens, CompletionTokens: p.maxTokens, TotalTokens: p.promptTokens + p.maxTokens},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Warn("failed to write response", "id", p.id, "error", err.Error())
	}
}

// tokenText is deterministic so tests can assert on exact output.
func tokenText(i int) string { return fmt.Sprintf("tok%d ", i) }

// writeEvent writes one SSE event and flushes it so the client sees it now.
func writeEvent(w io.Writer, f http.Flusher, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return err
	}
	f.Flush()
	return nil
}

// sleepCtx sleeps for d and reports false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

func writeError(w http.ResponseWriter, status int, msg string) {
	errType := "invalid_request_error"
	if status >= http.StatusInternalServerError {
		errType = "server_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Message: msg, Type: errType}})
}
