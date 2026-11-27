package fakebackend

import (
	"errors"
	"fmt"
	"time"
)

// Config controls how the fake backend behaves. Every field can be overridden
// per request for the three timing knobs (see the X-Fake-* headers in server.go).
type Config struct {
	// ModelName is reported by /v1/models and used when a request names no model.
	ModelName string
	// FirstTokenDelay simulates prefill: the time between a request starting to
	// run and its first token being emitted.
	FirstTokenDelay time.Duration
	// TokensPerSecond is the decode rate after the first token. 0 means no pacing.
	TokensPerSecond float64
	// DefaultMaxTokens is used when a request does not set max_tokens.
	DefaultMaxTokens int
	// MaxConcurrency is how many requests may generate at once. Extra requests
	// wait, which is what the "waiting" gauge reports. 0 means unlimited.
	MaxConcurrency int
}

// DefaultConfig returns the defaults used by cmd/fakebackend.
func DefaultConfig() Config {
	return Config{
		ModelName:        "fake-llm",
		FirstTokenDelay:  500 * time.Millisecond,
		TokensPerSecond:  50,
		DefaultMaxTokens: 64,
		MaxConcurrency:   8,
	}
}

// Validate reports the first invalid field, if any.
func (c Config) Validate() error {
	switch {
	case c.ModelName == "":
		return errors.New("model name must not be empty")
	case c.FirstTokenDelay < 0:
		return fmt.Errorf("first token delay must be >= 0, got %s", c.FirstTokenDelay)
	case c.TokensPerSecond < 0:
		return fmt.Errorf("tokens per second must be >= 0, got %g", c.TokensPerSecond)
	case c.DefaultMaxTokens < 1 || c.DefaultMaxTokens > maxAllowedTokens:
		return fmt.Errorf("default max tokens must be between 1 and %d, got %d", maxAllowedTokens, c.DefaultMaxTokens)
	case c.MaxConcurrency < 0:
		return fmt.Errorf("max concurrency must be >= 0, got %d", c.MaxConcurrency)
	}
	return nil
}
