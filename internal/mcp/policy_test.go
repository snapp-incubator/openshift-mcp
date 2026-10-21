package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

func cpol(name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kyverno.io/v1", "kind": "ClusterPolicy",
		"metadata": map[string]any{"name": name}, "spec": spec,
	}}
}

func validateRule(name string, match map[string]any, extra map[string]any) map[string]any {
	r := map[string]any{"name": name, "match": match,
		"validate": map[string]any{"message": name + " message", "pattern": map[string]any{"spec": "x"}}}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func anyKinds(kinds ...string) map[string]any {
	ks := make([]any, len(kinds))
	for i, k := range kinds {
		ks[i] = k
	}
	return map[string]any{"any": []any{map[string]any{"resources": map[string]any{"kinds": ks}}}}
}

func policyClient(t *testing.T, objs ...runtime.Object) *k8s.Client {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{"team": "a"}}}
	listKinds := map[schema.GroupVersionResource]string{
		gvrClusterPolicy: "ClusterPolicyList", gvrPolicy: "PolicyList",
		gvrExceptions[0]: "PolicyExceptionList", gvrExceptions[1]: "PolicyExceptionList",
		gvrPolicyReport: "PolicyReportList",
	}
	return &k8s.Client{
		Clientset: fake.NewSimpleClientset(ns),
		Dynamic:   dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...),
	}
}

func rulesByName(t *testing.T, out any) (map[string]map[string]any, string) {
	t.Helper()
	b, _ := json.Marshal(out)
	var res struct {
		Rules []map[string]any `json:"rules"`
	}
	_ = json.Unmarshal(b, &res)
	m := map[string]map[string]any{}
	for _, r := range res.Rules {
		m[r["policy"].(string)] = r
	}
	return m, string(b)
}

func TestListPoliciesKeepsOnlyWhatApplies(t *testing.T) {
	c := policyClient(t,
		cpol("require-limits", map[string]any{"validationFailureAction": "Enforce",
			"rules": []any{validateRule("limits", anyKinds("Pod"), nil)}}),
		cpol("restrict-nodeport", map[string]any{"validationFailureAction": "enforce",
			"rules": []any{validateRule("nodeport", anyKinds("Service"), nil)}}),
		cpol("sre-only", map[string]any{"validationFailureAction": "Enforce",
			"rules": []any{validateRule("sre", map[string]any{"any": []any{map[string]any{
				"resources": map[string]any{"kinds": []any{"Pod"}, "namespaces": []any{"sre-*"}}}}}, nil)}}),
		cpol("not-team-a", map[string]any{"validationFailureAction": "Enforce",
			"rules": []any{validateRule("ex", anyKinds("Pod"), map[string]any{"exclude": map[string]any{"any": []any{
				map[string]any{"resources": map[string]any{"namespaces": []any{"team-a"}}}}}})}}),
		cpol("admins-exempt", map[string]any{"validationFailureAction": "Audit",
			"rules": []any{validateRule("labels", anyKinds("Pod"), map[string]any{"exclude": map[string]any{"any": []any{
				map[string]any{"clusterRoles": []any{"cluster-admin"}}}}})}}),
		cpol("team-b-only", map[string]any{"validationFailureAction": "Enforce",
			"rules": []any{validateRule("b", map[string]any{"any": []any{map[string]any{"resources": map[string]any{
				"kinds": []any{"Pod"}, "namespaceSelector": map[string]any{"matchLabels": map[string]any{"team": "b"}}}}}}, nil)}}),
		cpol("overridden", map[string]any{"validationFailureAction": "Audit",
			"validationFailureActionOverrides": []any{map[string]any{"action": "Enforce", "namespaces": []any{"team-*"}}},
			"rules":                            []any{validateRule("o", anyKinds("apps/v1/Deployment"), nil)}}),
	)
	out, err := handleListPolicies(context.Background(), c, args{"namespace": "team-a", "kind": "Deployment"})
	if err != nil {
		t.Fatal(err)
	}
	got, raw := rulesByName(t, out)

	if r := got["require-limits"]; r == nil || r["action"] != "Enforce" || r["applies_via"] == nil {
		t.Errorf("a Pod rule must apply to a Deployment via autogen, Enforce: %v", r)
	}
	if r := got["admins-exempt"]; r == nil || r["action"] != "Audit" {
		t.Errorf("a rule excluding only admins still applies to a user: %v", r)
	}
	if r := got["overridden"]; r == nil || r["action"] != "Enforce" {
		t.Errorf("the namespace override must turn Audit into Enforce: %v", r)
	}
	for _, p := range []string{"restrict-nodeport", "sre-only", "not-team-a", "team-b-only"} {
		if got[p] != nil {
			t.Errorf("%s does not apply to a Deployment in team-a", p)
		}
	}
	// The match/exclude of other policies is never echoed.
	for _, leak := range []string{"sre-", "team-b", `"team":"b"`} {
		if strings.Contains(raw, leak) {
			t.Errorf("output names another namespace (%q): %s", leak, raw)
		}
	}
	if first, _ := json.Marshal(out); !strings.Contains(string(first), `"check":"{`) {
		t.Errorf("the check must be a JSON string, not a nested object: %s", first)
	}
}

func TestListPoliciesMarksExemptions(t *testing.T) {
	ex := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kyverno.io/v2", "kind": "PolicyException",
		"metadata": map[string]any{"name": "team-a-limits", "namespace": "kyverno"},
		"spec": map[string]any{
			"exceptions": []any{map[string]any{"policyName": "require-limits", "ruleNames": []any{"limits"}}},
			"match": map[string]any{"any": []any{map[string]any{"resources": map[string]any{
				"kinds": []any{"Deployment"}, "namespaces": []any{"team-a"}}}}},
		},
	}}
	c := policyClient(t, ex, cpol("require-limits", map[string]any{"validationFailureAction": "Enforce",
		"rules": []any{validateRule("limits", anyKinds("Pod"), nil)}}))
	out, err := handleListPolicies(context.Background(), c, args{"namespace": "team-a", "kind": "Deployment"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := rulesByName(t, out)
	if got["require-limits"]["exempted"] == nil {
		t.Fatalf("exception not reflected: %v", got["require-limits"])
	}
}

func TestPolicyReportListsViolationsFirst(t *testing.T) {
	rep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wgpolicyk8s.io/v1alpha2", "kind": "PolicyReport",
		"metadata": map[string]any{"name": "r1", "namespace": "team-a"},
		"scope":    map[string]any{"kind": "Deployment", "name": "web"},
		"results": []any{
			map[string]any{"policy": "p", "rule": "ok", "result": "pass"},
			map[string]any{"policy": "probes", "rule": "liveness", "result": "warn", "message": "add probes"},
			map[string]any{"policy": "limits", "rule": "mem", "result": "fail", "message": "set limits"},
		},
	}}
	out, err := handleGetPolicyReport(context.Background(), policyClient(t, rep), args{"namespace": "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var res struct {
		Totals  map[string]int   `json:"totals"`
		Results []map[string]any `json:"results"`
	}
	_ = json.Unmarshal(b, &res)
	if len(res.Results) != 2 || res.Results[0]["result"] != "fail" || res.Results[0]["name"] != "web" {
		t.Fatalf("results: %+v", res.Results)
	}
	if res.Totals["pass"] != 1 || res.Totals["fail"] != 1 {
		t.Fatalf("totals: %v", res.Totals)
	}
}
