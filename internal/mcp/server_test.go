package mcp

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

func testPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "team-a", Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "web:1"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Ready: false, RestartCount: 7,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff", Message: "back-off restarting failed container",
				}},
			}},
		},
	}
}

func connect(t *testing.T) *sdkmcp.ClientSession {
	t.Helper()
	return connectWith(t, fake.NewSimpleClientset(testPod()))
}

func connectWith(t *testing.T, cs *fake.Clientset) *sdkmcp.ClientSession {
	t.Helper()
	return connectClient(t, &k8s.Client{Clientset: cs})
}

func connectClient(t *testing.T, client *k8s.Client) *sdkmcp.ClientSession {
	t.Helper()
	srv := NewServer(client, slog.Default())
	ts := httptest.NewServer(srv.HTTPHandler())
	t.Cleanup(ts.Close)

	mc := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "v0"}, nil)
	session, err := mc.Connect(context.Background(),
		&sdkmcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call(t *testing.T, session *sdkmcp.ClientSession, name string, arguments map[string]any) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name: name, Arguments: arguments,
	})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	text := res.Content[0].(*sdkmcp.TextContent).Text
	if res.IsError {
		t.Fatalf("call %s returned tool error: %s", name, text)
	}
	return text
}

func requireContains(t *testing.T, tool, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s output missing %q:\n%s", tool, want, text)
		}
	}
}

var allToolNames = []string{
	"list_pods", "get_pod", "pod_logs", "list_events", "list_workloads",
	"get_workload", "list_services", "get_service", "list_routes",
	"list_pvcs", "get_quota", "top_pods", "list_nodes", "get_node",
	"get_resource", "list_resource",
	"diagnose_namespace", "list_config", "list_network_policies",
	"list_ingresses", "list_rbac", "list_machines",
	"list_hpas", "list_pdbs", "list_namespaces",
	"api_resources", "list_cluster_operators",
	"get_cluster_version", "list_builds", "list_imagestreams",
}

const maxTools = 30

func TestToolCountWithinBudget(t *testing.T) {
	if got := len(buildTools()); got > maxTools {
		t.Errorf("got %d tools, budget is %d — fold the new capability into an existing tool instead", got, maxTools)
	}
}

func TestOriginalToolNamesUnchanged(t *testing.T) {
	session := connect(t)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{
		"list_pods", "get_pod", "pod_logs", "list_events", "list_workloads",
		"get_workload", "list_services", "get_service", "list_routes",
		"list_pvcs", "get_quota", "top_pods", "list_nodes", "get_node",
		"get_resource", "list_resource",
	} {
		if !names[want] {
			t.Errorf("original tool %s was removed or renamed — this breaks existing callers", want)
		}
	}
}

func TestNamespacedToolsRequireNamespace(t *testing.T) {
	clusterScoped := map[string]bool{
		"list_nodes": true, "get_node": true, "list_namespaces": true,
		"api_resources": true, "list_cluster_operators": true,
		"get_cluster_version": true, "list_machines": true,
		"get_resource": true, "list_resource": true,
	}
	for _, tl := range buildTools() {
		if clusterScoped[tl.name] {
			continue
		}
		props, _ := tl.schema["properties"].(map[string]any)
		if _, ok := props["namespace"]; !ok {
			t.Errorf("tool %s has no namespace property", tl.name)
			continue
		}
		required, _ := tl.schema["required"].([]string)
		found := false
		for _, r := range required {
			if r == "namespace" {
				found = true
			}
		}
		if !found {
			t.Errorf("tool %s does not require namespace", tl.name)
		}
	}
}

func TestToolsRegistered(t *testing.T) {
	session := connect(t)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(res.Tools) != len(allToolNames) {
		t.Errorf("got %d tools, want %d", len(res.Tools), len(allToolNames))
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range allToolNames {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
}

func TestToolsHaveDescriptionAndSchema(t *testing.T) {
	session := connect(t)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tool := range res.Tools {
		if len(tool.Description) < 20 {
			t.Errorf("tool %s has a too-short description: %q", tool.Name, tool.Description)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %s has no input schema", tool.Name)
		}
	}
}

func TestListPodsShowsCrashLoop(t *testing.T) {
	session := connect(t)
	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "list_pods",
		Arguments: map[string]any{"namespace": "team-a"},
	})
	if err != nil {
		t.Fatalf("call list_pods: %v", err)
	}
	text := res.Content[0].(*sdkmcp.TextContent).Text
	for _, want := range []string{"web-0", "CrashLoopBackOff", `"restarts": 7`, "node-1"} {
		if !strings.Contains(text, want) {
			t.Errorf("list_pods output missing %q:\n%s", want, text)
		}
	}
}

func TestGetPodIncludesContainerState(t *testing.T) {
	session := connect(t)
	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "get_pod",
		Arguments: map[string]any{"namespace": "team-a", "name": "web-0"},
	})
	if err != nil {
		t.Fatalf("call get_pod: %v", err)
	}
	text := res.Content[0].(*sdkmcp.TextContent).Text
	if !strings.Contains(text, "back-off restarting failed container") {
		t.Errorf("get_pod missing waiting message:\n%s", text)
	}
}

func TestResultsAreNotHTMLEscaped(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "web"},
			Ports: []corev1.ServicePort{{
				Port: 80, TargetPort: intstr.FromInt32(8080), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	session := connectWith(t, fake.NewSimpleClientset(svc))
	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "list_services",
		Arguments: map[string]any{"namespace": "team-a"},
	})
	if err != nil {
		t.Fatalf("call list_services: %v", err)
	}
	text := res.Content[0].(*sdkmcp.TextContent).Text
	if !strings.Contains(text, "80->8080/TCP") {
		t.Errorf("expected a plain arrow in output:\n%s", text)
	}
	escapedGT := "\\u003e"
	if strings.Contains(text, escapedGT) {
		t.Errorf("output must not be HTML-escaped:\n%s", text)
	}
}

func TestRenderJSONLeavesAngleBracketsAlone(t *testing.T) {
	got, err := renderJSON(map[string]any{"rule": "a -> b & c < d"})
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	if !strings.Contains(got, "a -> b & c < d") {
		t.Errorf("renderJSON escaped its input: %s", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("renderJSON must not leave a trailing newline: %q", got)
	}
}

func TestErrorsAreToolErrorsNotProtocolErrors(t *testing.T) {
	session := connect(t)
	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "get_pod",
		Arguments: map[string]any{"namespace": "team-a", "name": "missing"},
	})
	if err != nil {
		t.Fatalf("expected tool-level error, got protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError result for a missing pod")
	}
}
