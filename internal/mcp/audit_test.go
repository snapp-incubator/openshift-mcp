package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

var (
	rabbitGVR      = schema.GroupVersionResource{Group: "rabbitmq.com", Version: "v1beta1", Resource: "rabbitmqclusters"}
	redisClusterV2 = schema.GroupVersionResource{Group: opstreeGroup, Version: "v1beta2", Resource: "redisclusters"}
	redisReplV1    = schema.GroupVersionResource{Group: opstreeGroup, Version: "v1beta1", Resource: "redisreplications"}
	teamGVR        = schema.GroupVersionResource{Group: "team.snappcloud.io", Version: "v1alpha1", Resource: "teams"}
)

// newDynamic builds a fake dynamic client that serves every GVR the audit
// lists. The fake panics on an unregistered list, so all are registered, and
// objs are added under their explicit GVR (the fake's kind→resource guess
// turns Redis into "redises").
func newDynamic(t *testing.T, objs map[schema.GroupVersionResource][]*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	listKinds := map[schema.GroupVersionResource]string{teamGVR: "TeamList"}
	for _, kind := range dataServiceKinds {
		for _, v := range kind.versions {
			listKinds[schema.GroupVersionResource{Group: kind.group, Version: v, Resource: kind.resource}] = kind.kind + "List"
		}
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	for gvr, list := range objs {
		for _, o := range list {
			if err := dyn.Tracker().Create(gvr, o, o.GetNamespace()); err != nil {
				t.Fatalf("seed %s: %v", gvr, err)
			}
		}
	}
	return dyn
}

// notServed makes the fake answer like an API server without the resource.
func notServed(dyn *dynamicfake.FakeDynamicClient, resource, version string) {
	dyn.PrependReactor("list", resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
		if version != "" && action.GetResource().Version != version {
			return false, nil, nil
		}
		return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), "")
	})
}

func cr(gvr schema.GroupVersionResource, kind, ns, name, uid string, spec map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": ns, "uid": uid},
	}}
	if spec != nil {
		o.Object["spec"] = spec
	}
	return o
}

func ptr[T any](v T) *T { return &v }

func controlledBy(kind, name, uid string) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: "v1", Kind: kind, Name: name, UID: types.UID(uid), Controller: ptr(true)}}
}

func deployment(ns, name string, replicas int32, image string, tmpl map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: tmpl},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: image}}},
		}},
	}
}

func statefulSet(ns, name string, replicas int32, image string, tmpl map[string]string, owners []metav1.OwnerReference) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, OwnerReferences: owners},
		Spec: appsv1.StatefulSetSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: tmpl},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: image}}},
		}},
	}
}

func pod(ns, name string, phase corev1.PodPhase, lbls map[string]string, owners []metav1.OwnerReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbls, OwnerReferences: owners},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func pdb(ns, name string, sel map[string]string, minAvail, maxUnavail *intstr.IntOrString, status policyv1.PodDisruptionBudgetStatus) *policyv1.PodDisruptionBudget {
	p := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: minAvail, MaxUnavailable: maxUnavail},
		Status:     status,
	}
	if sel != nil {
		p.Spec.Selector = &metav1.LabelSelector{MatchLabels: sel}
	}
	return p
}

type auditResult struct {
	PDBs         []auditPDB         `json:"pdbs"`
	Instances    []auditInstance    `json:"instances"`
	Namespaces   map[string]nsOwner `json:"namespaces"`
	NotInstalled []string           `json:"notInstalled"`
	QuorumImages []string           `json:"quorumImages"`
}

func runAudit(t *testing.T, c *k8s.Client, a args) auditResult {
	t.Helper()
	out, err := handleDisruptionAudit(context.Background(), c, a)
	if err != nil {
		t.Fatalf("disruption_audit: %v", err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var res auditResult
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	return res
}

func auditFixture(t *testing.T) *k8s.Client {
	t.Helper()
	one := intstr.FromInt32(1)
	zero := intstr.FromString("0%")
	app := map[string]string{"app": "api"}
	web := map[string]string{"app": "web"}
	rmq := map[string]string{"app.kubernetes.io/name": "events"}
	leader := map[string]string{"app": "cache-leader"}

	apiDeploy := deployment("pay-prod", "api", 1, "registry.local/pay/api:1", app)
	apiDeploy.Annotations = map[string]string{annotationIgnore: "pdb-overlap"}

	cs := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pay-prod", Labels: map[string]string{labelTeam: "pay", labelEnv: "production"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "plat"}},

		// A single-replica Deployment selected by two PDBs.
		apiDeploy,
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "pay-prod", Name: "api-7d9f", OwnerReferences: controlledBy("Deployment", "api", "d1")}},
		pod("pay-prod", "api-7d9f-x", corev1.PodRunning, app, controlledBy("ReplicaSet", "api-7d9f", "rs1")),
		pdb("pay-prod", "api-pdb", app, &one, nil, policyv1.PodDisruptionBudgetStatus{ExpectedPods: 1, CurrentHealthy: 1, DesiredHealthy: 1}),
		pdb("pay-prod", "api-pdb-2", app, nil, &zero, policyv1.PodDisruptionBudgetStatus{ExpectedPods: 1, CurrentHealthy: 1, DesiredHealthy: 1}),

		// A pending pod in two PDBs: eviction never checks it, so no overlap.
		pod("pay-prod", "web-0", corev1.PodPending, web, nil),
		pdb("pay-prod", "web-a", web, &one, nil, policyv1.PodDisruptionBudgetStatus{}),
		pdb("pay-prod", "web-b", web, &one, nil, policyv1.PodDisruptionBudgetStatus{}),
		// A finished pod is ignored entirely.
		pod("pay-prod", "job-x", corev1.PodSucceeded, app, nil),

		// A nil selector matches nothing.
		pdb("plat", "nothing", nil, &one, nil, policyv1.PodDisruptionBudgetStatus{}),

		// Operator-owned members.
		statefulSet("pay-prod", "events-server", 1, "rabbitmq:3.13", rmq, controlledBy("RabbitmqCluster", "events", "rmq-uid")),
		pdb("pay-prod", "events-pdb", rmq, &one, nil, policyv1.PodDisruptionBudgetStatus{}),
		statefulSet("pay-prod", "cache-follower", 3, "redis:7", map[string]string{"app": "cache-follower"}, controlledBy("RedisCluster", "cache", "rc-uid")),
		statefulSet("pay-prod", "cache-leader", 3, "redis:7", leader, controlledBy("RedisCluster", "cache", "rc-uid")),

		// Plain workloads, detected by image.
		statefulSet("plat", "kafka", 3, "docker.io/bitnami/kafka:3.6@sha256:abc", map[string]string{"app": "kafka"}, nil),
		statefulSet("plat", "sentinel", 3, "bitnami/redis-sentinel:7", map[string]string{"app": "sentinel"}, nil),
		deployment("plat", "redis", 1, "redis:7", map[string]string{"app": "redis"}),
		deployment("plat", "redis-exporter", 1, "oliver006/redis_exporter:v1", nil),
		deployment("plat", "etcd", 1, "quay.io/coreos/etcd:v3", nil), // a Deployment is no member set
	)

	dyn := newDynamic(t, map[schema.GroupVersionResource][]*unstructured.Unstructured{
		rabbitGVR:      {cr(rabbitGVR, "RabbitmqCluster", "pay-prod", "events", "rmq-uid", nil)},
		redisClusterV2: {cr(redisClusterV2, "RedisCluster", "pay-prod", "cache", "rc-uid", map[string]any{"clusterSize": int64(3)})},
		redisReplV1:    {cr(redisReplV1, "RedisReplication", "plat", "sessions", "rr-uid", map[string]any{"clusterSize": int64(1)})},
	})
	notServed(dyn, "redis", "")                    // no standalone Redis CRD at all
	notServed(dyn, "redisreplications", "v1beta2") // an older operator: v1beta1 only
	return &k8s.Client{Clientset: cs, Dynamic: dyn}
}

func TestDisruptionAuditPDBs(t *testing.T) {
	res := runAudit(t, auditFixture(t), nil)
	byName := map[string]auditPDB{}
	for _, p := range res.PDBs {
		byName[p.Name] = p
	}
	if len(byName) != 6 {
		t.Fatalf("got %d PDBs, want 6: %+v", len(byName), res.PDBs)
	}

	api := byName["api-pdb"]
	wantOwner := auditOwner{Kind: "Deployment", Name: "api", Replicas: ptr(int32(1)),
		Images: []string{"registry.local/pay/api:1"}, Ignore: "pdb-overlap"}
	if len(api.Owners) != 1 || !reflect.DeepEqual(api.Owners[0], wantOwner) {
		t.Errorf("api-pdb owners = %+v, want [%+v] (resolved through the ReplicaSet)", api.Owners, wantOwner)
	}
	if !reflect.DeepEqual(api.OverlapsWith, []string{"api-pdb-2"}) {
		t.Errorf("api-pdb overlapsWith = %v, want [api-pdb-2]", api.OverlapsWith)
	}
	if api.MinAvailable == nil || api.MinAvailable.IntValue() != 1 || api.DesiredHealthy != 1 || api.ExpectedPods != 1 {
		t.Errorf("api-pdb spec/status not passed through: %+v", api)
	}
	if mu := byName["api-pdb-2"].MaxUnavailable; mu == nil || mu.String() != "0%" {
		t.Errorf("api-pdb-2 maxUnavailable = %v, want \"0%%\"", mu)
	}

	if w := byName["web-a"]; len(w.OverlapsWith) != 0 || len(w.Owners) != 1 || w.Owners[0].Kind != "Pod" {
		t.Errorf("pending pod: want an owner but no overlap, got %+v", w)
	}
	if n := byName["nothing"]; len(n.Owners) != 0 {
		t.Errorf("nil selector must match nothing, got owners %+v", n.Owners)
	}
	if e := byName["events-pdb"]; len(e.Owners) != 0 {
		t.Errorf("events-pdb has no live pods, got owners %+v", e.Owners)
	}
}

func TestDisruptionAuditInstances(t *testing.T) {
	res := runAudit(t, auditFixture(t), nil)
	got := map[string]auditInstance{}
	for _, inst := range res.Instances {
		got[inst.Namespace+"/"+inst.Name+"/"+inst.Kind] = inst
	}

	want := map[string]auditInstance{
		"pay-prod/events/RabbitmqCluster": {Namespace: "pay-prod", Name: "events", Engine: "rabbitmq", Kind: "RabbitmqCluster",
			Workload: "events-server", Replicas: 1, Quorum: true, Source: "operator", PDBs: []string{"events-pdb"}},
		"pay-prod/cache/RedisCluster": {Namespace: "pay-prod", Name: "cache", Engine: "redis", Kind: "RedisCluster",
			Workload: "cache-leader", Replicas: 3, Quorum: true, Source: "operator", PDBs: []string{}},
		"plat/sessions/RedisReplication": {Namespace: "plat", Name: "sessions", Engine: "redis", Kind: "RedisReplication",
			Replicas: 1, Source: "operator", PDBs: []string{}},
		"plat/kafka/StatefulSet": {Namespace: "plat", Name: "kafka", Engine: "kafka", Kind: "StatefulSet",
			Replicas: 3, Quorum: true, Source: "image", PDBs: []string{}, Images: []string{"docker.io/bitnami/kafka:3.6@sha256:abc"}},
		"plat/sentinel/StatefulSet": {Namespace: "plat", Name: "sentinel", Engine: "redis-sentinel", Kind: "StatefulSet",
			Replicas: 3, Quorum: true, Source: "image", PDBs: []string{}, Images: []string{"bitnami/redis-sentinel:7"}},
		"plat/redis/Deployment": {Namespace: "plat", Name: "redis", Engine: "redis", Kind: "Deployment",
			Replicas: 1, Source: "image", PDBs: []string{}, Images: []string{"redis:7"}},
	}
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("instances mismatch, got:\n%s", gb)
	}

	if !reflect.DeepEqual(res.NotInstalled, []string{"redis." + opstreeGroup}) {
		t.Errorf("notInstalled = %v, want only the standalone Redis CRD", res.NotInstalled)
	}
	if res.Namespaces["pay-prod"] != (nsOwner{Team: "pay", Env: "production"}) {
		t.Errorf("pay-prod owner = %+v", res.Namespaces["pay-prod"])
	}
	if o, ok := res.Namespaces["plat"]; !ok || o != (nsOwner{}) {
		t.Errorf("unlabelled namespace must be present and empty, got %+v (present=%v)", o, ok)
	}
}

func TestDisruptionAuditCustomQuorumImages(t *testing.T) {
	res := runAudit(t, auditFixture(t), args{"quorum_images": []any{"ZooKeeper"}})
	for _, inst := range res.Instances {
		if inst.Name == "kafka" {
			t.Errorf("kafka is not in the custom quorum list, still reported: %+v", inst)
		}
	}
	if !reflect.DeepEqual(res.QuorumImages, []string{"zookeeper"}) {
		t.Errorf("quorumImages = %v", res.QuorumImages)
	}
	if defaultQuorumImages[0] != "rabbitmq" {
		t.Error("custom images must not overwrite the defaults")
	}
}

func TestDisruptionAuditFailsLoudly(t *testing.T) {
	c := auditFixture(t)
	c.Dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "rabbitmqclusters",
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(action.GetResource().GroupResource(), "", nil)
		})
	if _, err := handleDisruptionAudit(context.Background(), c, nil); err == nil {
		t.Fatal("a forbidden CRD list must fail the audit, not drop instances silently")
	}
}

func TestDetectEngine(t *testing.T) {
	kw := append([]string{}, cacheEngines...)
	kw = append(kw, defaultQuorumImages...)
	for images, want := range map[string]string{
		"redis:7":                        "redis",
		"localhost:5000/bitnami/redis":   "redis",
		"bitnami/redis-sentinel:7.2":     "redis-sentinel",
		"oliver006/redis_exporter:v1":    "",
		"percona/percona-server-mongodb": "mongo",
		"nginx:1.27":                     "",
	} {
		if got := detectEngine([]string{images}, kw); got != want {
			t.Errorf("detectEngine(%q) = %q, want %q", images, got, want)
		}
	}
}

func TestListTeams(t *testing.T) {
	dyn := newDynamic(t, map[schema.GroupVersionResource][]*unstructured.Unstructured{teamGVR: {
		{Object: map[string]any{
			"apiVersion": "team.snappcloud.io/v1alpha1", "kind": "Team",
			"metadata": map[string]any{"name": "pay"},
			"spec": map[string]any{
				"teamAdmins": []any{map[string]any{"name": "a@snapp.cab"}, map[string]any{"name": "b@snapp.cab"}},
				"projects":   []any{map[string]any{"name": "pay-prod", "envLabel": "production"}},
			},
		}},
		{Object: map[string]any{"apiVersion": "team.snappcloud.io/v1alpha1", "kind": "Team", "metadata": map[string]any{"name": "empty"}}},
	}})
	out, err := handleListTeams(context.Background(), &k8s.Client{Dynamic: dyn}, nil)
	if err != nil {
		t.Fatalf("list_teams: %v", err)
	}
	b, _ := json.Marshal(out)
	want := `{"count":2,"teams":[{"team":"empty","admins":[],"projects":[]},` +
		`{"team":"pay","admins":["a@snapp.cab","b@snapp.cab"],"projects":[{"name":"pay-prod","envLabel":"production"}]}]}`
	if string(b) != want {
		t.Errorf("list_teams =\n%s\nwant\n%s", b, want)
	}

	notServed(dyn, "teams", "")
	if _, err := handleListTeams(context.Background(), &k8s.Client{Dynamic: dyn}, nil); err == nil {
		t.Error("a missing Team CRD must be an error: no teams means no recipients")
	}
}
