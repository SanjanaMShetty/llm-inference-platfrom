package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"llm-platform/api/v1alpha1"
)

// TestManagerWiring runs the real manager so the watches are exercised: a change
// to the Deployment status or to a pod must reach the InferenceService status
// without anyone calling Reconcile by hand.
func TestManagerWiring(t *testing.T) {
	requireEnvtest(t)

	mgr, err := ctrl.NewManager(testCfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if err := (&InferenceServiceReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mgr.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	ns := newNamespace(t)
	name := "wired"
	key := keyOf(ns, name)
	createISVC(t, ns, name, nil)

	phase := func() v1alpha1.Phase {
		var isvc v1alpha1.InferenceService
		if err := k8sClient.Get(bg(), key, &isvc); err != nil {
			return ""
		}
		return isvc.Status.Phase
	}
	const wait = 20 * time.Second

	// 1. Creating the InferenceService makes the manager create children and a status.
	eventually(t, wait, "Deployment and Loading status", func() bool {
		var dep appsv1.Deployment
		return k8sClient.Get(bg(), key, &dep) == nil && phase() == v1alpha1.PhaseLoading
	})

	// 2. A Deployment status change (watched via Owns) flips the phase to Ready.
	setDeploymentStatus(t, key, func(s *appsv1.DeploymentStatus) {
		s.Replicas, s.ReadyReplicas, s.AvailableReplicas, s.UpdatedReplicas = 1, 1, 1, 1
	})
	eventually(t, wait, "phase Ready after the Deployment became ready", func() bool { return phase() == v1alpha1.PhaseReady })

	// 3. The Deployment loses readiness and a pod becomes unschedulable. The pod
	// event (watched by label, since a ReplicaSet owns pods) updates the phase.
	setDeploymentStatus(t, key, func(s *appsv1.DeploymentStatus) {
		s.Replicas, s.ReadyReplicas, s.AvailableReplicas, s.UpdatedReplicas = 1, 0, 0, 1
	})
	eventually(t, wait, "phase Loading after readiness was lost", func() bool { return phase() == v1alpha1.PhaseLoading })

	createPod(t, ns, name, name+"-pod", corev1.PodStatus{
		Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/2 nodes are available: 2 Insufficient cpu.",
		}},
	})
	eventually(t, wait, "phase Unschedulable after a pod event", func() bool { return phase() == v1alpha1.PhaseUnschedulable })
}
