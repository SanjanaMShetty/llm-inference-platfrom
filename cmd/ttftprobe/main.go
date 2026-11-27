// Command ttftprobe sends streaming chat requests and reports time-to-first-token
// and decode rate. It exists to validate the fake backend (and later the gateway)
// with the same measurement code every time.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type result struct {
	ttft    time.Duration
	tokens  int
	rate    float64 // tokens/second between first and last content chunk
	total   time.Duration
	sawDone bool
	err     error
}

func main() { os.Exit(run()) }

func run() int {
	baseURL := flag.String("url", "http://localhost:8000", "base URL of the backend or gateway")
	n := flag.Int("n", 5, "number of requests")
	c := flag.Int("c", 1, "concurrent requests")
	maxTokens := flag.Int("max-tokens", 32, "max_tokens per request")
	expectTTFT := flag.Duration("expect-ttft", 0, "if set, fail unless median TTFT matches this (within -tolerance)")
	expectRate := flag.Float64("expect-rate", 0, "if set, fail unless median tokens/s matches this (within -tolerance)")
	tolerance := flag.Float64("tolerance", 0.25, "relative tolerance for the expectations")
	flag.Parse()

	if *n < 1 || *c < 1 || *maxTokens < 1 {
		fmt.Fprintln(os.Stderr, "n, c and max-tokens must be >= 1")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := &http.Client{} // no client-wide Timeout: it would cut streams short

	results := make([]result, *n)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < *c; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = runOne(ctx, client, *baseURL, *maxTokens)
			}
		}()
	}
	for i := 0; i < *n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	fmt.Printf("%-4s %-12s %-7s %-9s %-12s %s\n", "#", "ttft", "tokens", "tokens/s", "total", "done")
	var ttfts, rates []float64
	failed := 0
	for i, r := range results {
		if r.err != nil || !r.sawDone {
			failed++
			reason := "no [DONE] event"
			if r.err != nil {
				reason = r.err.Error()
			}
			fmt.Printf("%-4d FAILED: %s\n", i+1, reason)
			continue
		}
		ttfts = append(ttfts, float64(r.ttft)/float64(time.Millisecond))
		rates = append(rates, r.rate)
		fmt.Printf("%-4d %-12s %-7d %-9.1f %-12s yes\n", i+1, r.ttft.Round(100*time.Microsecond), r.tokens, r.rate, r.total.Round(time.Millisecond))
	}
	if failed > 0 {
		fmt.Printf("\n%d of %d requests failed\n", failed, *n)
		return 1
	}

	medTTFT := median(ttfts)
	medRate := median(rates)
	fmt.Printf("\nmedian ttft: %.1fms   median tokens/s: %.1f   (n=%d, c=%d)\n", medTTFT, medRate, *n, *c)

	status := 0
	if *expectTTFT > 0 {
		want := float64(*expectTTFT) / float64(time.Millisecond)
		low, high := want*(1-*tolerance), want*(1+*tolerance)+20 // +20ms absolute slack for network and scheduling
		if medTTFT < low || medTTFT > high {
			fmt.Printf("FAIL: median TTFT %.1fms outside [%.1f, %.1f]ms\n", medTTFT, low, high)
			status = 1
		} else {
			fmt.Printf("PASS: median TTFT %.1fms within [%.1f, %.1f]ms\n", medTTFT, low, high)
		}
	}
	if *expectRate > 0 {
		low, high := *expectRate*(1-*tolerance), *expectRate*(1+*tolerance)
		if medRate < low || medRate > high {
			fmt.Printf("FAIL: median rate %.1f tokens/s outside [%.1f, %.1f]\n", medRate, low, high)
			status = 1
		} else {
			fmt.Printf("PASS: median rate %.1f tokens/s within [%.1f, %.1f]\n", medRate, low, high)
		}
	}
	return status
}

func runOne(ctx context.Context, client *http.Client, baseURL string, maxTokens int) result {
	var r result
	body := fmt.Sprintf(`{"model":"probe","stream":true,"max_tokens":%d,"messages":[{"role":"user","content":"hello world"}]}`, maxTokens)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		r.err = err
		return r
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		r.err = err
		return r
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		r.err = fmt.Errorf("unexpected status %d", resp.StatusCode)
		return r
	}

	var first, last time.Time
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		payload, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if payload == "[DONE]" {
			r.sawDone = true
			break
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			r.err = fmt.Errorf("bad event JSON: %w", err)
			return r
		}
		if len(ch.Choices) == 0 || ch.Choices[0].Delta.Content == "" {
			continue
		}
		now := time.Now()
		if first.IsZero() {
			first = now
			r.ttft = now.Sub(start)
		}
		last = now
		r.tokens++
	}
	if err := sc.Err(); err != nil {
		r.err = err
	}
	r.total = time.Since(start)
	if r.tokens > 1 && last.After(first) {
		r.rate = float64(r.tokens-1) / last.Sub(first).Seconds()
	}
	return r
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
