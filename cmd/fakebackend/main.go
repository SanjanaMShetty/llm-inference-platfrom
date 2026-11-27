// Command fakebackend runs the fake OpenAI-compatible model server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"llm-platform/internal/fakebackend"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakebackend:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := fakebackend.DefaultConfig()
	addr := flag.String("addr", envString("FAKE_ADDR", ":8000"), "listen address (env FAKE_ADDR)")
	flag.StringVar(&cfg.ModelName, "model", envString("FAKE_MODEL", cfg.ModelName), "model name reported to clients (env FAKE_MODEL)")

	firstTokenDelay, err := envDuration("FAKE_FIRST_TOKEN_DELAY", cfg.FirstTokenDelay)
	if err != nil {
		return err
	}
	rate, err := envFloat("FAKE_TOKENS_PER_SECOND", cfg.TokensPerSecond)
	if err != nil {
		return err
	}
	maxTokens, err := envInt("FAKE_DEFAULT_MAX_TOKENS", cfg.DefaultMaxTokens)
	if err != nil {
		return err
	}
	maxConc, err := envInt("FAKE_MAX_CONCURRENCY", cfg.MaxConcurrency)
	if err != nil {
		return err
	}
	flag.DurationVar(&cfg.FirstTokenDelay, "first-token-delay", firstTokenDelay, "delay before the first token (env FAKE_FIRST_TOKEN_DELAY)")
	flag.Float64Var(&cfg.TokensPerSecond, "tokens-per-second", rate, "decode rate after the first token, 0 = unpaced (env FAKE_TOKENS_PER_SECOND)")
	flag.IntVar(&cfg.DefaultMaxTokens, "default-max-tokens", maxTokens, "max_tokens when a request omits it (env FAKE_DEFAULT_MAX_TOKENS)")
	flag.IntVar(&cfg.MaxConcurrency, "max-concurrency", maxConc, "concurrent generations, 0 = unlimited (env FAKE_MAX_CONCURRENCY)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv, err := fakebackend.New(cfg, logger)
	if err != nil {
		return err
	}

	// Deliberately no ReadTimeout or WriteTimeout: a server-wide write timeout
	// would cut off long-running SSE streams. Only the header read is bounded.
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	logger.Info("fakebackend listening",
		"addr", *addr,
		"model", cfg.ModelName,
		"first_token_delay", cfg.FirstTokenDelay.String(),
		"tokens_per_second", cfg.TokensPerSecond,
		"max_concurrency", cfg.MaxConcurrency)

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down, waiting for in-flight streams")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func envFloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
