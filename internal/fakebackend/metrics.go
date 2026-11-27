package fakebackend

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// handleMetrics writes Prometheus text exposition by hand to avoid a dependency.
//
// The vllm:* names mimic the gauges a real vLLM server exposes so the autoscaler
// and scheduler can be exercised without a GPU. Real vLLM metric names change
// between versions: reconcile these with the version you pin before M4.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	running := s.running.Load()
	waiting := s.waiting.Load()
	cacheUsage := 0.0
	if s.cfg.MaxConcurrency > 0 {
		cacheUsage = float64(running) / float64(s.cfg.MaxConcurrency)
	}

	var b strings.Builder
	metric := func(name, help, typ string, value any) {
		_, _ = fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, typ, name, value)
	}
	metric("vllm:num_requests_running", "Requests currently generating tokens.", "gauge", running)
	metric("vllm:num_requests_waiting", "Requests waiting for a generation slot.", "gauge", waiting)
	metric("vllm:gpu_cache_usage_perc", "Fake KV-cache usage: running divided by max concurrency.", "gauge", cacheUsage)
	metric("fakebackend_max_concurrency", "Configured maximum concurrent generations (0 = unlimited).", "gauge", s.cfg.MaxConcurrency)
	metric("fakebackend_requests_total", "Chat completion requests accepted.", "counter", s.requestsTotal.Load())
	metric("fakebackend_tokens_generated_total", "Tokens generated.", "counter", s.tokensTotal.Load())
	metric("fakebackend_client_disconnects_total", "Requests abandoned by the client before completion.", "counter", s.disconnects.Load())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}
