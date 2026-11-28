package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"llm-platform/api/v1alpha1"
)

func testISVC() *v1alpha1.InferenceService {
	return &v1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"},
		Spec: v1alpha1.InferenceServiceSpec{
			Model:                 "fake-llm",
			Backend:               v1alpha1.BackendFake,
			Image:                 "fakebackend:dev",
			Classification:        v1alpha1.ClassificationInternal,
			WeightLoadTimeSeconds: 300,
			Autoscaling:           &v1alpha1.AutoscalingSpec{MinReplicas: 1, MaxReplicas: 1},
		},
	}
}

func TestSelectorIsMinimalAndMatchesPods(t *testing.T) {
	dep, err := buildDeployment(testISVC())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{labelInstance: "demo"}
	if !reflect.DeepEqual(dep.Spec.Selector.MatchLabels, want) {
		t.Fatalf("selector = %v, want exactly %v (it is immutable, so it must hold only the service name)", dep.Spec.Selector.MatchLabels, want)
	}
	for k, v := range want {
		if dep.Spec.Template.Labels[k] != v {
			t.Errorf("pod template is missing selector label %s=%s", k, v)
		}
	}
	// Mutable facts live in labels outside the selector.
	if dep.Spec.Template.Labels[labelBackend] != "fake" {
		t.Errorf("backend label = %q", dep.Spec.Template.Labels[labelBackend])
	}
}

func TestDeploymentNeverSetsReplicas(t *testing.T) {
	dep, err := buildDeployment(testISVC())
	if err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Replicas != nil {
		t.Fatalf("buildDeployment set replicas=%d; under server-side apply the operator must never mention it", *dep.Spec.Replicas)
	}
}

func TestExactlyOneContainerPerBackend(t *testing.T) {
	cases := []struct {
		backend   v1alpha1.Backend
		port      int32
		path      string
		firstArg  string
		wantEnv   string
		wantNoEnv bool
	}{
		{v1alpha1.BackendFake, 8000, "/healthz", "", "FAKE_MODEL", false},
		{v1alpha1.BackendVLLM, 8000, "/health", "facebook/opt-125m", "", true},
		{v1alpha1.BackendOllama, 11434, "/", "serve", "", true},
	}
	for _, tc := range cases {
		t.Run(string(tc.backend), func(t *testing.T) {
			isvc := testISVC()
			isvc.Spec.Backend = tc.backend
			isvc.Spec.Model = "facebook/opt-125m"
			dep, err := buildDeployment(isvc)
			if err != nil {
				t.Fatal(err)
			}
			cs := dep.Spec.Template.Spec.Containers
			if len(cs) != 1 || cs[0].Name != containerName {
				t.Fatalf("containers = %+v, want exactly one named %q", cs, containerName)
			}
			c := cs[0]
			if c.Ports[0].ContainerPort != tc.port || c.Ports[0].Name != portName {
				t.Errorf("port = %+v, want %s=%d", c.Ports[0], portName, tc.port)
			}
			if got := c.ReadinessProbe.HTTPGet.Path; got != tc.path {
				t.Errorf("health path = %q, want %q", got, tc.path)
			}
			if tc.firstArg != "" && (len(c.Args) == 0 || c.Args[0] != tc.firstArg) {
				t.Errorf("args = %v, want first arg %q", c.Args, tc.firstArg)
			}
			if tc.wantNoEnv && len(c.Env) != 0 {
				t.Errorf("env = %v, want none", c.Env)
			}
			if tc.wantEnv != "" && (len(c.Env) != 1 || c.Env[0].Name != tc.wantEnv || c.Env[0].Value != "facebook/opt-125m") {
				t.Errorf("env = %v, want %s=<model>", c.Env, tc.wantEnv)
			}
		})
	}
}

func TestUnknownBackendIsAnError(t *testing.T) {
	isvc := testISVC()
	isvc.Spec.Backend = "tensorflow"
	if _, err := buildDeployment(isvc); err == nil {
		t.Fatal("expected an error for an unsupported backend")
	}
}

func TestExtraArgsAreAppendedAfterBackendArgs(t *testing.T) {
	isvc := testISVC()
	isvc.Spec.Backend = v1alpha1.BackendVLLM
	isvc.Spec.Model = "m"
	isvc.Spec.ExtraArgs = []string{"--max-model-len", "2048"}
	dep, err := buildDeployment(isvc)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"m", "--host", "0.0.0.0", "--port", "8000", "--max-model-len", "2048"}
	if got := dep.Spec.Template.Spec.Containers[0].Args; !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestStartupFailureThresholdCoversWeightLoadTime(t *testing.T) {
	cases := []struct {
		seconds int32
		want    int32
	}{
		{0, 60}, // unset falls back to the 300s default
		{1, 1},
		{5, 1},
		{6, 2},
		{30, 6},
		{300, 60},
		{3600, 720},
	}
	for _, tc := range cases {
		got := startupFailureThreshold(tc.seconds)
		if got != tc.want {
			t.Errorf("startupFailureThreshold(%d) = %d, want %d", tc.seconds, got, tc.want)
		}
		if tc.seconds > 0 && got*probePeriodSeconds < tc.seconds {
			t.Errorf("window %ds is shorter than the declared %ds weight-load time", got*probePeriodSeconds, tc.seconds)
		}
	}
}

func TestProbesAreAllPresentAndStartupFollowsSpec(t *testing.T) {
	isvc := testISVC()
	isvc.Spec.WeightLoadTimeSeconds = 120
	dep, err := buildDeployment(isvc)
	if err != nil {
		t.Fatal(err)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.StartupProbe == nil || c.ReadinessProbe == nil || c.LivenessProbe == nil {
		t.Fatal("startup, readiness and liveness probes must all be set")
	}
	if c.StartupProbe.FailureThreshold != 24 || c.StartupProbe.PeriodSeconds != 5 {
		t.Errorf("startup probe = %+v, want 24 x 5s for a 120s load time", c.StartupProbe)
	}
	if c.ReadinessProbe.HTTPGet.Port != intstr.FromString(portName) {
		t.Errorf("readiness port = %v", c.ReadinessProbe.HTTPGet.Port)
	}
}

func TestResourcesMapToRequestsAndMemoryLimit(t *testing.T) {
	isvc := testISVC()
	cpu, mem := resource.MustParse("500m"), resource.MustParse("2Gi")
	isvc.Spec.Resources = &v1alpha1.ResourceHints{CPU: &cpu, Memory: &mem}
	dep, err := buildDeployment(isvc)
	if err != nil {
		t.Fatal(err)
	}
	rr := dep.Spec.Template.Spec.Containers[0].Resources
	if rr.Requests.Cpu().Cmp(cpu) != 0 || rr.Requests.Memory().Cmp(mem) != 0 {
		t.Errorf("requests = %v", rr.Requests)
	}
	if rr.Limits.Memory().Cmp(mem) != 0 {
		t.Errorf("memory limit = %v, want %v", rr.Limits.Memory(), mem)
	}
	if _, ok := rr.Limits[corev1.ResourceCPU]; ok {
		t.Error("a CPU limit would throttle the model server; none should be set")
	}
}

func TestNoResourcesWhenUnset(t *testing.T) {
	dep, err := buildDeployment(testISVC())
	if err != nil {
		t.Fatal(err)
	}
	rr := dep.Spec.Template.Spec.Containers[0].Resources
	if len(rr.Requests) != 0 || len(rr.Limits) != 0 {
		t.Errorf("resources = %+v, want none", rr)
	}
}

func TestServiceSelectsPodsAndExposesNamedPort(t *testing.T) {
	svc := buildService(testISVC())
	if !reflect.DeepEqual(svc.Spec.Selector, selectorLabels("demo")) {
		t.Errorf("selector = %v", svc.Spec.Selector)
	}
	p := svc.Spec.Ports[0]
	if p.Port != 80 || p.TargetPort != intstr.FromString(portName) {
		t.Errorf("port = %+v, want 80 -> %s", p, portName)
	}
	if svc.Name != "demo" || svc.Namespace != "ns" {
		t.Errorf("service identity = %s/%s", svc.Namespace, svc.Name)
	}
}

func TestEndpoint(t *testing.T) {
	if got := endpointFor(testISVC()); got != "http://demo.ns.svc:80" {
		t.Errorf("endpoint = %q", got)
	}
}

func TestInitialReplicas(t *testing.T) {
	isvc := testISVC()
	isvc.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MinReplicas: 3, MaxReplicas: 5}
	if got := initialReplicas(isvc); got != 3 {
		t.Errorf("initialReplicas = %d, want 3", got)
	}
	isvc.Spec.Autoscaling = nil
	if got := initialReplicas(isvc); got != 1 {
		t.Errorf("initialReplicas without autoscaling = %d, want 1", got)
	}
}

func TestSecurityContextIsRestrictive(t *testing.T) {
	dep, err := buildDeployment(testISVC())
	if err != nil {
		t.Fatal(err)
	}
	sc := dep.Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("privilege escalation must be disabled: %+v", sc)
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities = %+v, want drop ALL", sc.Capabilities)
	}
}
