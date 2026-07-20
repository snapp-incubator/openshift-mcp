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
	client := &k8s.Client{Clientset: fake.NewSimpleClientset(testPod())}
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

func TestToolsRegistered(t *testing.T) {
	session := connect(t)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(res.Tools) != 17 {
		t.Errorf("got %d tools, want 17", len(res.Tools))
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{
		"list_pods", "get_pod", "pod_logs", "list_events", "list_workloads",
		"get_workload", "list_services", "get_service", "list_routes",
		"list_pvcs", "get_quota", "top_pods", "list_nodes", "get_node",
		"get_resource", "list_resource", "list_certificates",
	} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
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
