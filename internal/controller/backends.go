package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"llm-platform/api/v1alpha1"
)

// backendProfile captures everything that differs between model servers.
type backendProfile struct {
	port       int32
	healthPath string
	args       func(spec *v1alpha1.InferenceServiceSpec) []string
	env        func(spec *v1alpha1.InferenceServiceSpec) []corev1.EnvVar
}

func profileFor(b v1alpha1.Backend) (backendProfile, error) {
	switch b {
	case v1alpha1.BackendFake:
		return backendProfile{
			port:       8000,
			healthPath: "/healthz",
			args:       func(*v1alpha1.InferenceServiceSpec) []string { return nil },
			env: func(spec *v1alpha1.InferenceServiceSpec) []corev1.EnvVar {
				return []corev1.EnvVar{{Name: "FAKE_MODEL", Value: spec.Model}}
			},
		}, nil

	case v1alpha1.BackendVLLM:
		return backendProfile{
			port:       8000,
			healthPath: "/health",
			// ASSUMPTION: the image's entrypoint is `vllm serve`, which takes the
			// model as its first argument. Verify against the vLLM image version you
			// pin (milestones M2 and M8); extraArgs lets you adjust without a code change.
			args: func(spec *v1alpha1.InferenceServiceSpec) []string {
				return []string{spec.Model, "--host", "0.0.0.0", "--port", "8000"}
			},
			env: func(*v1alpha1.InferenceServiceSpec) []corev1.EnvVar { return nil },
		}, nil

	case v1alpha1.BackendOllama:
		return backendProfile{
			port:       11434,
			healthPath: "/",
			// Pulling the model after start-up is handled by the model-cache milestone.
			args: func(*v1alpha1.InferenceServiceSpec) []string { return []string{"serve"} },
			env:  func(*v1alpha1.InferenceServiceSpec) []corev1.EnvVar { return nil },
		}, nil
	}
	return backendProfile{}, fmt.Errorf("unsupported backend %q", b)
}
