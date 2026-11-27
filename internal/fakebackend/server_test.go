package fakebackend

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func baseConfig() Config {
	return Config{
		ModelName:        "test-model",
		FirstTokenDelay:  0,
		TokensPerSecond:  0,
		DefaultMaxTokens: 8,
		MaxConcurrency:   0,
	}
}

func newTestServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func chatBody(stream bool, maxTokens int, includeUsage bool) string {
	return fmt.Sprintf(
		`{"model":"m","stream":%t,"max_tokens":%d,"stream_options":{"include_usage":%t},"messages":[{"role":"user","content":"hello there world"}]}`,
		stream, maxTokens, includeUsage)
}

func post(ctx context.Context, base, body string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

type sseEvent struct {
	data string
	at   time.Time
}

// readEvents collects "data: " lines with their arrival time. It returns
// whatever it read plus any read error (an aborted stream yields one).
func readEvents(r io.Reader) ([]sseEvent, error) {
	var events []sseEvent
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if payload, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			events = append(events, sseEvent{data: payload, at: time.Now()})
		}
	}
	return events, sc.Err()
}

func deltaContent(data string) string {
	var c struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &c); err != nil || len(c.Choices) == 0 {
		return ""
	}
	return c.Choices[0].Delta.Content
}

func countContent(events []sseEvent) int {
	n := 0
	for _, e := range events {
		if deltaContent(e.data) != "" {
			n++
		}
	}
	return n
}

func hasDone(events []sseEvent) bool {
	for _, e := range events {
		if e.data == "[DONE]" {
			return true
		}
	}
	return false
}

func metricValue(t *testing.T, base, name string) float64 {
	t.Helper()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == name {
			v, err := strconv.ParseFloat(f[1], 64)
			if err != nil {
				t.Fatalf("parse metric %s=%q: %v", name, f[1], err)
			}
			return v
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestHealthzAndModels(t *testing.T) {
	ts := newTestServer(t, baseConfig())

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "test-model" {
		t.Fatalf("unexpected models: %+v", out)
	}
}

func TestStreamingChatCompletion(t *testing.T) {
	cfg := baseConfig()
	cfg.FirstTokenDelay = 60 * time.Millisecond
	cfg.TokensPerSecond = 200 // 5ms between tokens
	ts := newTestServer(t, cfg)

	start := time.Now()
	resp, err := post(context.Background(), ts.URL, chatBody(true, 10, true), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}

	events, err := readEvents(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	elapsed := time.Since(start)

	if !hasDone(events) || events[len(events)-1].data != "[DONE]" {
		t.Fatalf("stream must end with [DONE]; got %d events", len(events))
	}
	if got := countContent(events); got != 10 {
		t.Fatalf("content chunks = %d, want 10", got)
	}

	// Time to first content chunk must respect the configured delay.
	var firstContent time.Time
	for _, e := range events {
		if deltaContent(e.data) != "" {
			firstContent = e.at
			break
		}
	}
	if ttft := firstContent.Sub(start); ttft < 55*time.Millisecond {
		t.Errorf("TTFT = %s, want >= 55ms (configured delay 60ms)", ttft)
	}
	// 60ms delay + 9 gaps of 5ms = 105ms; allow a little slack below.
	if elapsed < 100*time.Millisecond {
		t.Errorf("stream finished in %s, want >= 100ms", elapsed)
	}

	// The chunk before [DONE] carries usage because include_usage was set.
	var u struct {
		Usage *usage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(events[len(events)-2].data), &u); err != nil {
		t.Fatal(err)
	}
	if u.Usage == nil || u.Usage.CompletionTokens != 10 || u.Usage.PromptTokens != 3 || u.Usage.TotalTokens != 13 {
		t.Errorf("unexpected usage chunk: %+v", u.Usage)
	}
}

func TestStreamingWithoutUsage(t *testing.T) {
	ts := newTestServer(t, baseConfig())
	resp, err := post(context.Background(), ts.URL, chatBody(true, 4, false), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	events, err := readEvents(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(events[len(events)-2].data, `"usage"`) {
		t.Errorf("usage chunk present although include_usage was false")
	}
	if !hasDone(events) {
		t.Errorf("missing [DONE]")
	}
}

func TestNonStreamingChatCompletion(t *testing.T) {
	ts := newTestServer(t, baseConfig())
	resp, err := post(context.Background(), ts.URL, chatBody(false, 3, false), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out completion
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if got := out.Choices[0].Message.Content; got != "tok0 tok1 tok2 " {
		t.Errorf("content = %q", got)
	}
	if out.Usage.CompletionTokens != 3 || out.Usage.PromptTokens != 3 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

func TestPerRequestOverrides(t *testing.T) {
	cfg := baseConfig()
	cfg.FirstTokenDelay = 5 * time.Second // would time the test out if not overridden
	ts := newTestServer(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := post(ctx, ts.URL, chatBody(false, 2, false), map[string]string{
		headerFirstTokenDelay: "10ms",
		headerTokenRate:       "1000",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestBadRequests(t *testing.T) {
	ts := newTestServer(t, baseConfig())
	cases := []struct {
		name    string
		body    string
		headers map[string]string
	}{
		{"invalid json", `{not json`, nil},
		{"zero max tokens", chatBody(false, 0, false), nil},
		{"huge max tokens", chatBody(false, 999999, false), nil},
		{"bad delay header", chatBody(false, 1, false), map[string]string{headerFirstTokenDelay: "soon"}},
		{"bad rate header", chatBody(false, 1, false), map[string]string{headerTokenRate: "abc"}},
		{"bad fail-after header", chatBody(true, 1, false), map[string]string{headerFailAfterTokens: "-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := post(context.Background(), ts.URL, tc.body, tc.headers)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestQueueGauges(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxConcurrency = 1
	cfg.FirstTokenDelay = 500 * time.Millisecond
	ts := newTestServer(t, cfg)

	var wg sync.WaitGroup
	fire := func() {
		defer wg.Done()
		resp, err := post(context.Background(), ts.URL, chatBody(true, 2, false), nil)
		if err != nil {
			t.Errorf("request: %v", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	wg.Add(1)
	go fire()
	eventually(t, 2*time.Second, "one request running", func() bool {
		return metricValue(t, ts.URL, "vllm:num_requests_running") == 1
	})

	wg.Add(1)
	go fire()
	eventually(t, 2*time.Second, "one request waiting", func() bool {
		return metricValue(t, ts.URL, "vllm:num_requests_waiting") == 1
	})
	if got := metricValue(t, ts.URL, "vllm:gpu_cache_usage_perc"); got != 1 {
		t.Errorf("cache usage = %v, want 1 with the only slot busy", got)
	}

	wg.Wait()
	eventually(t, 2*time.Second, "gauges back to zero", func() bool {
		return metricValue(t, ts.URL, "vllm:num_requests_running") == 0 &&
			metricValue(t, ts.URL, "vllm:num_requests_waiting") == 0
	})
	if got := metricValue(t, ts.URL, "fakebackend_requests_total"); got != 2 {
		t.Errorf("requests_total = %v, want 2", got)
	}
}

func TestClientDisconnectFreesSlot(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxConcurrency = 1
	cfg.TokensPerSecond = 20
	ts := newTestServer(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	resp, err := post(ctx, ts.URL, chatBody(true, 1000, false), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	// Read until the first content chunk, then walk away mid-stream.
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if payload, ok := strings.CutPrefix(sc.Text(), "data: "); ok && deltaContent(payload) != "" {
			break
		}
	}
	cancel()
	_ = resp.Body.Close()

	eventually(t, 3*time.Second, "slot released after disconnect", func() bool {
		return metricValue(t, ts.URL, "vllm:num_requests_running") == 0
	})
	eventually(t, 3*time.Second, "disconnect counted", func() bool {
		return metricValue(t, ts.URL, "fakebackend_client_disconnects_total") >= 1
	})

	// The freed slot must be usable immediately.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	resp2, err := post(ctx2, ts.URL, chatBody(false, 1, false), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("follow-up status = %d", resp2.StatusCode)
	}
}

func TestFailAfterTokensTruncatesStream(t *testing.T) {
	cfg := baseConfig()
	cfg.TokensPerSecond = 500
	ts := newTestServer(t, cfg)

	resp, err := post(context.Background(), ts.URL, chatBody(true, 10, false),
		map[string]string{headerFailAfterTokens: "3"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A read error is expected but its exact form is not asserted.
	events, _ := readEvents(resp.Body)
	if hasDone(events) {
		t.Errorf("an aborted stream must not contain [DONE]")
	}
	if got := countContent(events); got != 3 {
		t.Errorf("content chunks before abort = %d, want 3", got)
	}
	eventually(t, 2*time.Second, "slot released after abort", func() bool {
		return metricValue(t, ts.URL, "vllm:num_requests_running") == 0
	})
}

func TestConfigValidate(t *testing.T) {
	bad := []Config{
		{},
		{ModelName: "m", DefaultMaxTokens: 0},
		{ModelName: "m", DefaultMaxTokens: 1, FirstTokenDelay: -1},
		{ModelName: "m", DefaultMaxTokens: 1, TokensPerSecond: -1},
		{ModelName: "m", DefaultMaxTokens: 1, MaxConcurrency: -1},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("default config invalid: %v", err)
	}
}
