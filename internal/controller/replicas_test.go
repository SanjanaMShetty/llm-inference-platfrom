package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func managed(manager, fieldsJSON string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{Manager: manager, FieldsV1: &metav1.FieldsV1{Raw: []byte(fieldsJSON)}}
}

func TestReplicasOwned(t *testing.T) {
	cases := []struct {
		name string
		mfs  []metav1.ManagedFieldsEntry
		want bool
	}{
		{"no managed fields", nil, false},
		{"operator owns the spec but not replicas",
			[]metav1.ManagedFieldsEntry{managed("llm-platform-operator", `{"f:spec":{"f:selector":{},"f:template":{}}}`)}, false},
		{"bootstrap owns replicas",
			[]metav1.ManagedFieldsEntry{
				managed("llm-platform-operator", `{"f:spec":{"f:selector":{}}}`),
				managed("llm-platform-operator-bootstrap", `{"f:spec":{"f:replicas":{}}}`),
			}, true},
		{"an autoscaler owns replicas",
			[]metav1.ManagedFieldsEntry{managed("kube-controller-manager", `{"f:spec":{"f:replicas":{}}}`)}, true},
		{"entry without fields is skipped",
			[]metav1.ManagedFieldsEntry{{Manager: "x"}}, false},
		{"malformed json is skipped",
			[]metav1.ManagedFieldsEntry{managed("x", `{not json`)}, false},
		{"metadata fields only",
			[]metav1.ManagedFieldsEntry{managed("x", `{"f:metadata":{"f:labels":{}}}`)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{ManagedFields: tc.mfs}}
			if got := replicasOwned(dep); got != tc.want {
				t.Errorf("replicasOwned = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPodToInferenceService(t *testing.T) {
	pod := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name: "demo-abc", Namespace: "ns", Labels: map[string]string{labelInstance: "demo"},
	}}
	reqs := podToInferenceService(context.Background(), pod)
	if len(reqs) != 1 || reqs[0].Name != "demo" || reqs[0].Namespace != "ns" {
		t.Fatalf("requests = %+v", reqs)
	}
	other := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"}}
	if got := podToInferenceService(context.Background(), other); got != nil {
		t.Errorf("unlabelled pod must map to nothing, got %+v", got)
	}
}
