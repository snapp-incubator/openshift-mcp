package mcp

import (
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

func testNode(name, cpu, memory string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(memory),
			},
		},
	}
}

func dynamicListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		clusterOperatorGVR:  "ClusterOperatorList",
		clusterVersionGVR:   "ClusterVersionList",
		buildGVR:            "BuildList",
		imageStreamGVR:      "ImageStreamList",
		deploymentConfigGVR: "DeploymentConfigList",
		machineGVR:          "MachineList",
		nodeMetricsGVR:      "NodeMetricsList",
		podMetricsGVR:       "PodMetricsList",
		routeGVR:            "RouteList",
	}
}

func connectDynamic(t *testing.T, objs ...runtime.Object) *sdkmcp.ClientSession {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), dynamicListKinds(), objs...)
	return connectClient(t, &k8s.Client{
		Clientset: fake.NewSimpleClientset(),
		Dynamic:   dyn,
	})
}

func unstructuredObj(apiVersion, kind, namespace, name string, spec, status map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
	}
	if namespace != "" {
		obj["metadata"].(map[string]any)["namespace"] = namespace
	}
	if spec != nil {
		obj["spec"] = spec
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

func conditions(entries ...map[string]any) []any {
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, e)
	}
	return out
}

func TestListClusterOperatorsSurfacesDegradedMessage(t *testing.T) {
	degraded := unstructuredObj("config.openshift.io/v1", "ClusterOperator", "", "ingress", nil,
		map[string]any{
			"conditions": conditions(
				map[string]any{"type": "Available", "status": "False", "message": "no router pods available"},
				map[string]any{"type": "Degraded", "status": "True", "message": "IngressControllerDegraded"},
			),
			"versions": []any{map[string]any{"name": "operator", "version": "4.14.1"}},
		})
	healthy := unstructuredObj("config.openshift.io/v1", "ClusterOperator", "", "dns", nil,
		map[string]any{
			"conditions": conditions(
				map[string]any{"type": "Available", "status": "True"},
				map[string]any{"type": "Degraded", "status": "False"},
			),
		})

	session := connectDynamic(t, degraded, healthy)
	text := call(t, session, "list_cluster_operators", nil)
	requireContains(t, "list_cluster_operators", text,
		"ingress", "IngressControllerDegraded", "no router pods available",
		"4.14.1", `"unhealthy_total": 1`, "dns")
}

func TestListClusterOperatorsUnhealthyOnly(t *testing.T) {
	degraded := unstructuredObj("config.openshift.io/v1", "ClusterOperator", "", "ingress", nil,
		map[string]any{"conditions": conditions(
			map[string]any{"type": "Available", "status": "True"},
			map[string]any{"type": "Degraded", "status": "True", "message": "broken"},
		)})
	healthy := unstructuredObj("config.openshift.io/v1", "ClusterOperator", "", "dns", nil,
		map[string]any{"conditions": conditions(
			map[string]any{"type": "Available", "status": "True"},
			map[string]any{"type": "Degraded", "status": "False"},
		)})

	session := connectDynamic(t, degraded, healthy)
	text := call(t, session, "list_cluster_operators", map[string]any{"unhealthy_only": true})
	requireContains(t, "list_cluster_operators", text, "ingress")
	if strings.Contains(text, "dns") {
		t.Errorf("unhealthy_only must exclude the healthy operator:\n%s", text)
	}
}

func TestGetClusterVersionReportsUpgradeState(t *testing.T) {
	cv := unstructuredObj("config.openshift.io/v1", "ClusterVersion", "", "version",
		map[string]any{"channel": "stable-4.14", "clusterID": "abc-123"},
		map[string]any{
			"desired": map[string]any{"version": "4.14.1"},
			"conditions": conditions(
				map[string]any{"type": "Available", "status": "True"},
				map[string]any{"type": "Progressing", "status": "True", "message": "Working towards 4.14.2: 45% complete"},
				map[string]any{"type": "Failing", "status": "False"},
			),
			"availableUpdates": []any{map[string]any{"version": "4.14.2"}},
			"history": []any{
				map[string]any{"version": "4.14.1", "state": "Completed", "startedTime": "2024-01-01T00:00:00Z"},
			},
		})

	session := connectDynamic(t, cv)
	text := call(t, session, "get_cluster_version", nil)
	requireContains(t, "get_cluster_version", text,
		"4.14.1", "stable-4.14", "45% complete", "4.14.2", "Completed")
}

func TestListBuildsSurfacesFailureReason(t *testing.T) {
	b := unstructuredObj("build.openshift.io/v1", "Build", "team-a", "app-3",
		map[string]any{"strategy": map[string]any{"type": "Docker"}},
		map[string]any{
			"phase":   "Failed",
			"reason":  "DockerBuildFailed",
			"message": "Dockerfile step failed",
		})
	session := connectDynamic(t, b)
	text := call(t, session, "list_builds", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_builds", text,
		"app-3", "Failed", "DockerBuildFailed", "Dockerfile step failed", "Docker")
}

func TestListImageStreamsFlagsUnresolvedTags(t *testing.T) {
	is := unstructuredObj("image.openshift.io/v1", "ImageStream", "team-a", "app", nil,
		map[string]any{
			"dockerImageRepository": "registry/team-a/app",
			"tags": []any{
				map[string]any{"tag": "good", "items": []any{map[string]any{"image": "sha256:abc"}}},
				map[string]any{"tag": "broken", "items": []any{}},
			},
		})
	session := connectDynamic(t, is)
	text := call(t, session, "list_imagestreams", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_imagestreams", text,
		"registry/team-a/app", "good", "broken", "unresolved_tags", "cannot pull")
}

func TestListWorkloadsIncludesDeploymentConfigs(t *testing.T) {
	dc := unstructuredObj("apps.openshift.io/v1", "DeploymentConfig", "team-a", "legacy",
		map[string]any{"replicas": int64(3)},
		map[string]any{
			"readyReplicas": int64(0),
			"latestVersion": int64(7),
			"conditions": conditions(
				map[string]any{"type": "Available", "status": "False", "message": "Deployment config has zero available replicas"},
			),
		})
	session := connectDynamic(t, dc)
	text := call(t, session, "list_workloads", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_workloads", text,
		"DeploymentConfig", "legacy", `"ready": "0/3"`,
		`"latest_version": 7`, "zero available replicas")
}

func TestListWorkloadsQuietWhenDeploymentConfigAPIAbsent(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset(testDeployment()))
	text := call(t, session, "list_workloads", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_workloads", text, "web")
	if strings.Contains(text, "warnings") {
		t.Errorf("a cluster without DeploymentConfigs must not produce warnings:\n%s", text)
	}
}

func TestListMachinesFlagsFailedAndNodelessMachines(t *testing.T) {
	failed := unstructuredObj("machine.openshift.io/v1beta1", "Machine", machineAPINamespace, "worker-1", nil,
		map[string]any{"phase": "Failed", "errorMessage": "instance terminated by provider"})
	running := unstructuredObj("machine.openshift.io/v1beta1", "Machine", machineAPINamespace, "worker-2", nil,
		map[string]any{"phase": "Running", "nodeRef": map[string]any{"name": "node-2"}})
	orphan := unstructuredObj("machine.openshift.io/v1beta1", "Machine", machineAPINamespace, "worker-3", nil,
		map[string]any{"phase": "Running"})

	session := connectDynamic(t, failed, running, orphan)
	text := call(t, session, "list_machines", nil)
	requireContains(t, "list_machines", text,
		"worker-1", "Failed", "instance terminated by provider",
		"worker-2", "node-2",
		"worker-3", "kubelet never joined",
		`"not_running_total": 1`)
}

func TestListMachinesUnhealthyOnly(t *testing.T) {
	failed := unstructuredObj("machine.openshift.io/v1beta1", "Machine", machineAPINamespace, "worker-1", nil,
		map[string]any{"phase": "Failed"})
	running := unstructuredObj("machine.openshift.io/v1beta1", "Machine", machineAPINamespace, "worker-2", nil,
		map[string]any{"phase": "Running", "nodeRef": map[string]any{"name": "node-2"}})

	session := connectDynamic(t, failed, running)
	text := call(t, session, "list_machines", map[string]any{"unhealthy_only": true})
	requireContains(t, "list_machines", text, "worker-1")
	if strings.Contains(text, "worker-2") {
		t.Errorf("unhealthy_only must exclude the Running machine:\n%s", text)
	}
}

func TestListRoutesReportsAdmission(t *testing.T) {
	r := unstructuredObj("route.openshift.io/v1", "Route", "team-a", "web", map[string]any{
		"host": "web.apps.example.com",
		"to":   map[string]any{"name": "web-svc"},
		"tls":  map[string]any{"termination": "edge"},
	}, map[string]any{
		"ingress": []any{map[string]any{
			"conditions": conditions(map[string]any{"type": "Admitted", "status": "True"}),
		}},
	})
	session := connectDynamic(t, r)
	text := call(t, session, "list_routes", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_routes", text,
		"web.apps.example.com", "web-svc", "edge", `"admitted": "True"`)
}

func nodeMetricsObj(name, cpu, memory string) *unstructured.Unstructured {
	obj := unstructuredObj("metrics.k8s.io/v1beta1", "Node", "", name, nil, nil)
	obj.Object["usage"] = map[string]any{"cpu": cpu, "memory": memory}
	return obj
}

func nodeUsageSession(t *testing.T, metrics *unstructured.Unstructured, nodes ...*corev1.Node) *sdkmcp.ClientSession {
	t.Helper()
	objs := make([]runtime.Object, 0, len(nodes))
	for _, n := range nodes {
		objs = append(objs, n)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), dynamicListKinds(), metrics)
	return connectClient(t, &k8s.Client{
		Clientset: fake.NewSimpleClientset(objs...),
		Dynamic:   dyn,
	})
}

func TestListNodesUsageIsPercentOfAllocatable(t *testing.T) {
	session := nodeUsageSession(t,
		nodeMetricsObj("node-1", "500m", "1024Mi"),
		testNode("node-1", "2", "4096Mi"))

	text := call(t, session, "list_nodes", map[string]any{"include_usage": true})
	requireContains(t, "list_nodes", text,
		"node-1", `"cpu_millicores": 500`, `"cpu_percent": "25%"`,
		`"memory_mi": 1024`, `"memory_percent": "25%"`)
}

func TestListNodesOmitsUsageByDefault(t *testing.T) {
	session := nodeUsageSession(t,
		nodeMetricsObj("node-1", "500m", "1024Mi"),
		testNode("node-1", "2", "4096Mi"))

	text := call(t, session, "list_nodes", nil)
	requireContains(t, "list_nodes", text, "node-1")
	if strings.Contains(text, "cpu_millicores") {
		t.Errorf("usage must be opt-in via include_usage:\n%s", text)
	}
}

func TestListNodesDegradesWhenMetricsUnavailable(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset(testNode("node-1", "2", "4096Mi")))
	text := call(t, session, "list_nodes", map[string]any{"include_usage": true})
	requireContains(t, "list_nodes", text, "node-1", "usage_warning")
}
