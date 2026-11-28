package controller

import (
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"llm-platform/api/v1alpha1"
)

// These tests talk only to the API server. No reconciler is running, which proves
// that invalid specs are rejected by the CRD schema and CEL rules themselves.

func baseSpec() map[string]interface{} {
	return map[string]interface{}{"model": "m", "backend": "fake", "image": "img:1"}
}

func rawISVC(ns, name string, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "InferenceService",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec":       spec,
	}}
}

func TestAdmission(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)

	restricted := func(backend string, accelerator map[string]interface{}) func(map[string]interface{}) {
		return func(s map[string]interface{}) {
			s["backend"] = backend
			s["classification"] = "Restricted"
			if accelerator != nil {
				s["accelerator"] = accelerator
			}
		}
	}
	args33 := make([]interface{}, 33)
	for i := range args33 {
		args33[i] = "a"
	}

	cases := []struct {
		name    string
		objName string
		mutate  func(map[string]interface{})
		wantErr string // empty means the object must be accepted
	}{
		{name: "valid minimal"},

		// CEL rule 1: Restricted needs a GPU-capable backend.
		{name: "rule1 restricted on fake", mutate: restricted("fake", map[string]interface{}{"deviceClassName": "gpu.example.com"}), wantErr: "GPU-capable backend"},
		{name: "rule1 restricted on ollama", mutate: restricted("Ollama", map[string]interface{}{"deviceClassName": "gpu.example.com"}), wantErr: "GPU-capable backend"},
		{name: "rule1 positive restricted vllm with device class", mutate: restricted("vLLM", map[string]interface{}{"deviceClassName": "gpu.example.com"})},

		// CEL rule 2: Restricted needs a named device class.
		{name: "rule2 restricted vllm without accelerator", mutate: restricted("vLLM", nil), wantErr: "accelerator.deviceClassName"},
		{name: "rule2 restricted vllm with shared claim only", mutate: restricted("vLLM", map[string]interface{}{"sharedClaimName": "shared"}), wantErr: "accelerator.deviceClassName"},

		// CEL rule 3: device class and shared claim are mutually exclusive.
		{name: "rule3 device class and shared claim", mutate: func(s map[string]interface{}) {
			s["accelerator"] = map[string]interface{}{"deviceClassName": "a", "sharedClaimName": "b"}
		}, wantErr: "mutually exclusive"},
		{name: "rule3 positive shared claim only", mutate: func(s map[string]interface{}) {
			s["accelerator"] = map[string]interface{}{"sharedClaimName": "b"}
		}},

		// CEL rule 4: max >= min.
		{name: "rule4 max below min", mutate: func(s map[string]interface{}) {
			s["autoscaling"] = map[string]interface{}{"minReplicas": int64(3), "maxReplicas": int64(2)}
		}, wantErr: "maxReplicas must be greater than or equal to minReplicas"},
		{name: "rule4 positive max above min", mutate: func(s map[string]interface{}) {
			s["autoscaling"] = map[string]interface{}{"minReplicas": int64(2), "maxReplicas": int64(4)}
		}},

		// CEL rule 5: the name also names the Service.
		{name: "rule5 name with dots", objName: "has.dots", wantErr: "DNS label"},
		{name: "rule5 name too long", objName: strings.Repeat("a", 64), wantErr: "DNS label"},
		{name: "rule5 positive 63 characters", objName: strings.Repeat("a", 63)},

		// OpenAPI schema constraints.
		{name: "schema min replicas zero", mutate: func(s map[string]interface{}) {
			s["autoscaling"] = map[string]interface{}{"minReplicas": int64(0), "maxReplicas": int64(2)}
		}, wantErr: "minReplicas"},
		{name: "schema unknown backend", mutate: func(s map[string]interface{}) { s["backend"] = "tensorflow" }, wantErr: "backend"},
		{name: "schema missing model", mutate: func(s map[string]interface{}) { delete(s, "model") }, wantErr: "model"},
		{name: "schema empty image", mutate: func(s map[string]interface{}) { s["image"] = "" }, wantErr: "image"},
		{name: "schema unknown classification", mutate: func(s map[string]interface{}) { s["classification"] = "Secret" }, wantErr: "classification"},
		{name: "schema weight load time too large", mutate: func(s map[string]interface{}) { s["weightLoadTimeSeconds"] = int64(99999) }, wantErr: "weightLoadTimeSeconds"},
		{name: "schema too many extra args", mutate: func(s map[string]interface{}) { s["extraArgs"] = args33 }, wantErr: "extraArgs"},
	}

	celNegatives, schemaNegatives := 0, 0
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.objName
			if name == "" {
				name = fmt.Sprintf("adm-%02d", i)
			}
			spec := baseSpec()
			if tc.mutate != nil {
				tc.mutate(spec)
			}
			err := k8sClient.Create(bg(), rawISVC(ns, name, spec))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected the object to be accepted, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected rejection containing %q, but the API server accepted the object", tc.wantErr)
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("expected an Invalid error, got: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.wantErr)
			}
			if strings.HasPrefix(tc.name, "rule") {
				celNegatives++
			} else {
				schemaNegatives++
			}
		})
	}
	t.Logf("admission summary: 5 CEL rules, %d CEL negative cases, %d schema negative cases (operator not running)", celNegatives, schemaNegatives)
}

func TestDefaultsAreApplied(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	createISVC(t, ns, "defaults", nil)
	got := getISVC(t, types.NamespacedName{Namespace: ns, Name: "defaults"})

	if got.Spec.Classification != v1alpha1.ClassificationInternal {
		t.Errorf("classification = %q, want Internal", got.Spec.Classification)
	}
	if got.Spec.Autoscaling == nil || got.Spec.Autoscaling.MinReplicas != 1 || got.Spec.Autoscaling.MaxReplicas != 1 {
		t.Errorf("autoscaling = %+v, want min 1 max 1", got.Spec.Autoscaling)
	}
	if got.Spec.WeightLoadTimeSeconds != 300 {
		t.Errorf("weightLoadTimeSeconds = %d, want 300", got.Spec.WeightLoadTimeSeconds)
	}
}

func TestAutoscalingDefaultsFillMissingBound(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	createISVC(t, ns, "partial", func(i *v1alpha1.InferenceService) {
		i.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MaxReplicas: 4} // minReplicas omitted
	})
	got := getISVC(t, types.NamespacedName{Namespace: ns, Name: "partial"})
	if got.Spec.Autoscaling.MinReplicas != 1 || got.Spec.Autoscaling.MaxReplicas != 4 {
		t.Errorf("autoscaling = %+v, want min 1 (defaulted) max 4", got.Spec.Autoscaling)
	}
}
