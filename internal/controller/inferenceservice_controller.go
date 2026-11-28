// Package controller reconciles InferenceService objects into a Deployment and a
// Service, and reports what the cluster is actually doing back in status.
package controller

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"llm-platform/api/v1alpha1"
)

const (
	// fieldOwner applies the full desired state, minus spec.replicas.
	fieldOwner = "llm-platform-operator"
	// fieldOwnerBootstrap applies spec.replicas exactly once, so that the operator
	// itself never owns it and an autoscaler can.
	fieldOwnerBootstrap = "llm-platform-operator-bootstrap"
)

// InferenceServiceReconciler reconciles InferenceService objects.
type InferenceServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=inference.llmplatform.dev,resources=inferenceservices,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=inference.llmplatform.dev,resources=inferenceservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one InferenceService toward its spec and records the result.
func (r *InferenceServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var isvc v1alpha1.InferenceService
	if err := r.Get(ctx, req.NamespacedName, &isvc); err != nil {
		// Deleted: the Deployment and Service carry owner references, so the
		// garbage collector removes them. No finalizer is needed.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !isvc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	dep, applyErr := r.applyChildren(ctx, &isvc)
	if applyErr != nil {
		logger.Error(applyErr, "failed to apply desired state")
	}

	var pods []corev1.Pod
	if dep != nil {
		var podList corev1.PodList
		if err := r.List(ctx, &podList,
			client.InNamespace(isvc.Namespace),
			client.MatchingLabels(selectorLabels(isvc.Name))); err != nil {
			return ctrl.Result{}, fmt.Errorf("list pods: %w", err)
		}
		pods = podList.Items
	}

	newStatus := buildStatus(isvc.Status, &isvc, observation{dep: dep, pods: pods, applyErr: applyErr})
	if !apiequality.Semantic.DeepEqual(isvc.Status, newStatus) {
		isvc.Status = newStatus
		if err := r.Status().Update(ctx, &isvc); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status: %w", err)
		}
	}

	// A failed apply is returned so controller-runtime retries with backoff.
	return ctrl.Result{}, applyErr
}

// applyChildren server-side-applies the Deployment and Service and returns the
// Deployment as the API server now holds it.
func (r *InferenceServiceReconciler) applyChildren(ctx context.Context, isvc *v1alpha1.InferenceService) (*appsv1.Deployment, error) {
	dep, err := buildDeployment(isvc)
	if err != nil {
		return nil, err
	}
	if err := controllerutil.SetControllerReference(isvc, dep, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner on Deployment: %w", err)
	}
	if err := r.Patch(ctx, dep, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
		return nil, fmt.Errorf("apply Deployment: %w", err)
	}
	// dep now holds the server's version, including managedFields and status.

	svc := buildService(isvc)
	if err := controllerutil.SetControllerReference(isvc, svc, r.Scheme); err != nil {
		return dep, fmt.Errorf("set owner on Service: %w", err)
	}
	if err := r.Patch(ctx, svc, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
		return dep, fmt.Errorf("apply Service: %w", err)
	}

	// Nobody owns spec.replicas yet: this Deployment was just created. Set the
	// initial count once, as a separate field manager. After this, an autoscaler
	// (or kubectl scale) owns it and the operator never touches it again.
	if !replicasOwned(dep) {
		replicas := initialReplicas(isvc)
		if err := r.bootstrapReplicas(ctx, dep, replicas); err != nil {
			return dep, fmt.Errorf("set initial replicas: %w", err)
		}
		dep.Spec.Replicas = &replicas
	}
	return dep, nil
}

func (r *InferenceServiceReconciler) bootstrapReplicas(ctx context.Context, dep *appsv1.Deployment, replicas int32) error {
	patch := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": appsv1.SchemeGroupVersion.String(),
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      dep.Name,
			"namespace": dep.Namespace,
		},
		"spec": map[string]interface{}{
			"replicas": int64(replicas),
		},
	}}
	return r.Patch(ctx, patch, client.Apply, client.FieldOwner(fieldOwnerBootstrap), client.ForceOwnership)
}

// replicasOwned reports whether any field manager owns spec.replicas.
func replicasOwned(dep *appsv1.Deployment) bool {
	for _, mf := range dep.ManagedFields {
		if mf.FieldsV1 == nil {
			continue
		}
		var fields map[string]interface{}
		if err := json.Unmarshal(mf.FieldsV1.Raw, &fields); err != nil {
			continue
		}
		spec, ok := fields["f:spec"].(map[string]interface{})
		if !ok {
			continue
		}
		if _, ok := spec["f:replicas"]; ok {
			return true
		}
	}
	return false
}

// podToInferenceService maps a pod event to the InferenceService that owns it.
func podToInferenceService(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[labelInstance]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

// SetupWithManager registers the controller and its watches.
func (r *InferenceServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("inferenceservice").
		// Only spec changes matter for the InferenceService itself; ignoring
		// status-only updates stops the operator reacting to its own writes.
		For(&v1alpha1.InferenceService{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		// Pods are owned by a ReplicaSet, not by us, so map them by label.
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(podToInferenceService)).
		Complete(r)
}
