package controller

import (
	"sort"
	"testing"
	"time"
)

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

// TestReconcileDurationReport measures how long a reconcile takes against the
// envtest API server and logs the numbers. It asserts nothing about speed (CI
// machines vary); run it with -v and record the output as evidence:
//
//	go test ./internal/controller -run TestReconcileDurationReport -v
func TestReconcileDurationReport(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	key := keyOf(ns, "perf")
	createISVC(t, ns, "perf", nil)
	r := newReconciler(k8sClient)

	start := time.Now()
	reconcileOnce(t, r, key)
	first := time.Since(start)

	reconcileOnce(t, r, key) // settle

	const n = 200
	durations := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		s := time.Now()
		reconcileOnce(t, r, key)
		durations = append(durations, time.Since(s))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	t.Logf("reconcile duration against envtest (uncached client, so every read is an API call)")
	t.Logf("  first reconcile (creates Deployment and Service, sets replicas, writes status): %s", first.Round(time.Millisecond))
	t.Logf("  steady state over %d reconciles: p50=%s p95=%s max=%s",
		n, percentile(durations, 0.50).Round(10*time.Microsecond), percentile(durations, 0.95).Round(10*time.Microsecond), durations[len(durations)-1].Round(10*time.Microsecond))
}
