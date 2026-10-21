package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

// Kyverno policies, for checking a manifest before it is applied. Rules are
// matched against the namespace here, so the answer lists only what applies
// and never names another tenant's namespace from a policy's match/exclude.

var (
	gvrClusterPolicy = schema.GroupVersionResource{Group: "kyverno.io", Version: "v1", Resource: "clusterpolicies"}
	gvrPolicy        = schema.GroupVersionResource{Group: "kyverno.io", Version: "v1", Resource: "policies"}
	gvrExceptions    = []schema.GroupVersionResource{
		{Group: "kyverno.io", Version: "v2", Resource: "policyexceptions"},
		{Group: "kyverno.io", Version: "v2beta1", Resource: "policyexceptions"},
	}
	gvrPolicyReport = schema.GroupVersionResource{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "policyreports"}
)

// podControllers get a Pod rule's checks through Kyverno's autogen.
var podControllers = map[string]bool{
	"Deployment": true, "StatefulSet": true, "DaemonSet": true, "ReplicaSet": true,
	"Job": true, "CronJob": true, "ReplicationController": true,
}

const maxCheckRunes = 1500

func handleListPolicies(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns, kind := a.str("namespace"), a.str("kind")
	nsObj, err := c.Clientset.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get namespace %s: %w", ns, err)
	}
	nsLabels := nsObj.Labels

	var policies []unstructured.Unstructured
	cps, err := c.Dynamic.Resource(gvrClusterPolicy).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list clusterpolicies: %w", err)
	}
	policies = append(policies, cps.Items...)
	if ps, err := c.Dynamic.Resource(gvrPolicy).Namespace(ns).List(ctx, metav1.ListOptions{}); err == nil {
		policies = append(policies, ps.Items...)
	}
	exempt := exemptions(ctx, c, ns, nsLabels, kind)

	var rules []map[string]any
	enforce := 0
	for i := range policies {
		p := &policies[i]
		spec, _, _ := unstructured.NestedMap(p.Object, "spec")
		ruleList, _, _ := unstructured.NestedSlice(p.Object, "spec", "rules")
		controllers := autogenControllers(p.GetAnnotations()["pod-policies.kyverno.io/autogen-controllers"])
		explicit := map[string]bool{}
		for _, r := range ruleList {
			if m, ok := r.(map[string]any); ok {
				if n, _ := m["name"].(string); strings.HasPrefix(n, "autogen-") {
					explicit[strings.TrimPrefix(strings.TrimPrefix(n, "autogen-"), "cronjob-")] = true
				}
			}
		}
		for _, r := range ruleList {
			rule, ok := r.(map[string]any)
			if !ok {
				continue
			}
			ctrl := controllers
			if n, _ := rule["name"].(string); explicit[n] {
				ctrl = nil // the policy already carries this rule's autogen form
			}
			kinds, applies, via, cond := ruleApplies(rule, ns, nsLabels, kind, ctrl)
			if !applies {
				continue
			}
			name, _ := rule["name"].(string)
			out := map[string]any{
				"policy": p.GetName(),
				"rule":   name,
				"type":   ruleType(rule),
				"kinds":  kinds,
			}
			if p.GetNamespace() != "" {
				out["scope"] = "Policy"
				out["namespace"] = p.GetNamespace()
			} else {
				out["scope"] = "ClusterPolicy"
			}
			if v, ok := rule["validate"].(map[string]any); ok {
				act := failureAction(spec, v, ns, nsLabels)
				out["action"] = act
				if act == "Enforce" {
					enforce++
				}
				if m, ok := v["message"].(string); ok {
					out["message"] = m
				}
				out["check"] = compactCheck(v)
			}
			if via != "" {
				out["applies_via"] = via
			}
			if cond != "" {
				out["only_if"] = cond
			}
			if exempt[p.GetName()+"/"+name] || exempt[p.GetName()+"/*"] {
				out["exempted"] = "a PolicyException exempts this namespace from the rule"
			}
			rules = append(rules, out)
		}
	}
	sort.SliceStable(rules, func(i, j int) bool {
		ai, aj := rules[i]["action"] == "Enforce", rules[j]["action"] == "Enforce"
		if ai != aj {
			return ai
		}
		return fmt.Sprint(rules[i]["policy"], rules[i]["rule"]) < fmt.Sprint(rules[j]["policy"], rules[j]["rule"])
	})
	return map[string]any{
		"namespace": ns,
		"kind":      kind,
		"rules":     rules,
		"summary": fmt.Sprintf("%d rules apply; %d are Enforce (a violating object is rejected), "+
			"the rest Audit (admitted, reported in the policy report) or mutate/generate", len(rules), enforce),
		"note": "check is the rule's own pattern, deny conditions or CEL, as JSON. Authoritative " +
			"validation is a server-side dry run: oc apply --dry-run=server -f <file>, which runs these policies.",
	}, nil
}

// ruleApplies reports whether a rule's match/exclude selects kind in ns, the
// kinds it names, how it applies (autogen) and any condition it depends on
// that cannot be decided without the object (names, labels, operations).
func ruleApplies(rule map[string]any, ns string, nsLabels map[string]string, kind string, autogen map[string]bool) (kinds []string, ok bool, via, cond string) {
	match, _ := rule["match"].(map[string]any)
	exclude, _ := rule["exclude"].(map[string]any)
	filters, all := resourceFilters(match)
	if len(filters) == 0 {
		return nil, false, "", ""
	}
	var conds []string
	matched := all
	for _, f := range filters {
		ks, okKind, v := kindMatches(f, kind, autogen)
		kinds = append(kinds, ks...)
		hit := okKind && nsMatches(f, ns, nsLabels)
		if hit {
			if v != "" {
				via = v
			}
			if c := objectCondition(f); c != "" {
				conds = append(conds, c)
			}
		}
		if all {
			matched = matched && hit
		} else if hit {
			matched = true
		}
	}
	if !matched {
		return kinds, false, "", ""
	}
	if pre, ok := rule["preconditions"]; ok && !isEmpty(pre) {
		conds = append(conds, "preconditions "+truncJSON(pre, 300))
	}
	// Exclude only what is certain for every object of this kind here: a
	// filter that also needs names, labels or a user is not a blanket exclusion.
	exFilters, exAll := resourceFilters(exclude)
	excluded := exAll && len(exFilters) > 0
	for _, f := range exFilters {
		_, okKind, _ := kindMatches(f, kind, autogen)
		hit := okKind && nsMatches(f, ns, nsLabels) && objectCondition(f) == "" && !hasSubjects(f)
		if exAll {
			excluded = excluded && hit
		} else if hit {
			excluded = true
		}
	}
	if excluded {
		return kinds, false, "", ""
	}
	return uniq(kinds), true, via, strings.Join(uniq(conds), "; ")
}

// resourceFilters returns a match/exclude block's filters and whether all must hold.
func resourceFilters(block map[string]any) ([]map[string]any, bool) {
	if block == nil {
		return nil, false
	}
	collect := func(key string) []map[string]any {
		var out []map[string]any
		list, _ := block[key].([]any)
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				if f := flattenFilter(m); len(f) > 0 {
					out = append(out, f)
				}
			}
		}
		return out
	}
	if f := collect("all"); len(f) > 0 {
		return f, true
	}
	if f := collect("any"); len(f) > 0 {
		return f, false
	}
	if _, ok := block["resources"]; ok { // legacy single filter
		if f := flattenFilter(block); len(f) > 0 {
			return []map[string]any{f}, true
		}
	}
	return nil, false
}

// flattenFilter merges a filter's resources block with its user fields.
func flattenFilter(f map[string]any) map[string]any {
	out := map[string]any{}
	if r, ok := f["resources"].(map[string]any); ok {
		for k, v := range r {
			if !isEmpty(v) {
				out[k] = v
			}
		}
	}
	for _, k := range []string{"subjects", "roles", "clusterRoles"} {
		if v, ok := f[k]; ok {
			out[k] = v
		}
	}
	return out
}

func kindMatches(f map[string]any, kind string, autogen map[string]bool) ([]string, bool, string) {
	var names []string
	ok, via := false, ""
	for _, k := range strSlice(f["kinds"]) {
		short := k
		if i := strings.LastIndex(k, "/"); i >= 0 {
			short = k[i+1:]
		}
		names = append(names, short)
		switch {
		case kind == "" || short == "*" || strings.EqualFold(short, kind):
			ok = true
		case short == "Pod" && autogen[kind]:
			ok, via = true, "autogen: the Pod rule is applied to the "+kind+"'s pod template"
		}
	}
	if len(names) == 0 { // no kinds: matches everything
		ok = true
	}
	return names, ok, via
}

func nsMatches(f map[string]any, ns string, nsLabels map[string]string) bool {
	if pats := strSlice(f["namespaces"]); len(pats) > 0 {
		hit := false
		for _, p := range pats {
			if m, _ := filepath.Match(p, ns); m {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if sel, ok := f["namespaceSelector"].(map[string]any); ok {
		return selectorMatches(sel, nsLabels)
	}
	return true
}

func selectorMatches(sel map[string]any, set map[string]string) bool {
	var ls metav1.LabelSelector
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(sel, &ls); err != nil {
		return true // unreadable selector: show the rule rather than hide it
	}
	s, err := metav1.LabelSelectorAsSelector(&ls)
	if err != nil {
		return true
	}
	return s.Matches(labels.Set(set))
}

// objectCondition describes a filter condition that depends on the object.
func objectCondition(f map[string]any) string {
	var c []string
	if n := strSlice(f["names"]); len(n) > 0 {
		c = append(c, "name matches "+strings.Join(n, ", "))
	}
	if sel, ok := f["selector"].(map[string]any); ok {
		b, _ := json.Marshal(sel)
		c = append(c, "labels match "+string(b))
	}
	if an, ok := f["annotations"].(map[string]any); ok {
		b, _ := json.Marshal(an)
		c = append(c, "annotations match "+string(b))
	}
	if ops := strSlice(f["operations"]); len(ops) > 0 {
		c = append(c, "operation is "+strings.Join(ops, "/"))
	}
	return strings.Join(c, ", ")
}

func hasSubjects(f map[string]any) bool {
	for _, k := range []string{"subjects", "roles", "clusterRoles"} {
		if l, ok := f[k].([]any); ok && len(l) > 0 {
			return true
		}
	}
	return false
}

// autogenControllers reads Kyverno's autogen annotation: unset means every pod
// controller, "none" none, otherwise the listed kinds.
func autogenControllers(ann string) map[string]bool {
	switch strings.TrimSpace(ann) {
	case "":
		return podControllers
	case "none":
		return nil
	}
	out := map[string]bool{}
	for _, k := range strings.Split(ann, ",") {
		out[strings.TrimSpace(k)] = true
	}
	return out
}

// isEmptyBlock is a rule block holding nothing but empty values.
func isEmptyBlock(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return isEmpty(v)
	}
	for _, x := range m {
		if !isEmptyBlock(x) {
			return false
		}
	}
	return true
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	case string:
		return t == ""
	}
	return false
}

func truncJSON(v any, n int) string {
	b, _ := json.Marshal(v)
	if r := []rune(string(b)); len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(b)
}

func ruleType(rule map[string]any) string {
	for _, t := range []string{"validate", "mutate", "generate", "verifyImages"} {
		if v, ok := rule[t]; ok && !isEmptyBlock(v) {
			return t
		}
	}
	return "other"
}

// failureAction resolves Enforce/Audit: the rule's own failureAction, then a
// namespace override, then the policy default (Audit when unset).
func failureAction(spec, validate map[string]any, ns string, nsLabels map[string]string) string {
	if a, ok := validate["failureAction"].(string); ok && a != "" {
		return normAction(a)
	}
	overrides, _ := spec["validationFailureActionOverrides"].([]any)
	for _, o := range overrides {
		m, ok := o.(map[string]any)
		if !ok {
			continue
		}
		if nsMatches(m, ns, nsLabels) && (len(strSlice(m["namespaces"])) > 0 || m["namespaceSelector"] != nil) {
			if a, ok := m["action"].(string); ok {
				return normAction(a)
			}
		}
	}
	if a, ok := spec["validationFailureAction"].(string); ok && a != "" {
		return normAction(a)
	}
	return "Audit"
}

func normAction(a string) string {
	if strings.EqualFold(a, "enforce") {
		return "Enforce"
	}
	return "Audit"
}

// compactCheck is the rule's check as a JSON string. A string, not a nested
// object, so keys inside a pattern are never read as fields of the record.
func compactCheck(validate map[string]any) string {
	check := map[string]any{}
	for _, k := range []string{"pattern", "anyPattern", "deny", "podSecurity", "cel", "foreach"} {
		if v, ok := validate[k]; ok {
			check[k] = v
		}
	}
	b, _ := json.Marshal(check)
	s := []rune(string(b))
	if len(s) > maxCheckRunes {
		return string(s[:maxCheckRunes]) + "…"
	}
	return string(s)
}

// exemptions returns "policy/rule" keys a PolicyException grants in ns.
func exemptions(ctx context.Context, c *k8s.Client, ns string, nsLabels map[string]string, kind string) map[string]bool {
	out := map[string]bool{}
	for _, gvr := range gvrExceptions {
		list, err := c.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, ex := range list.Items {
			match, _, _ := unstructured.NestedMap(ex.Object, "spec", "match")
			filters, all := resourceFilters(match)
			hit := all && len(filters) > 0
			for _, f := range filters {
				_, okKind, _ := kindMatches(f, kind, podControllers)
				h := okKind && nsMatches(f, ns, nsLabels)
				if all {
					hit = hit && h
				} else if h {
					hit = true
				}
			}
			if !hit {
				continue
			}
			entries, _, _ := unstructured.NestedSlice(ex.Object, "spec", "exceptions")
			for _, e := range entries {
				m, _ := e.(map[string]any)
				pol, _ := m["policyName"].(string)
				rules := strSlice(m["ruleNames"])
				if len(rules) == 0 {
					out[pol+"/*"] = true
				}
				for _, r := range rules {
					out[pol+"/"+r] = true
				}
			}
		}
		return out
	}
	return out
}

const maxReportResults = 200

func handleGetPolicyReport(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	list, err := c.Dynamic.Resource(gvrPolicyReport).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list policyreports in %s: %w", ns, err)
	}
	counts := map[string]int{}
	var results []map[string]any
	for _, rep := range list.Items {
		scopeKind, _, _ := unstructured.NestedString(rep.Object, "scope", "kind")
		scopeName, _, _ := unstructured.NestedString(rep.Object, "scope", "name")
		res, _, _ := unstructured.NestedSlice(rep.Object, "results")
		for _, r := range res {
			m, ok := r.(map[string]any)
			if !ok {
				continue
			}
			result, _ := m["result"].(string)
			counts[result]++
			if result == "pass" || result == "skip" {
				continue
			}
			kind, name := scopeKind, scopeName
			if rs, ok := m["resources"].([]any); ok && len(rs) > 0 {
				if o, ok := rs[0].(map[string]any); ok {
					kind, _ = o["kind"].(string)
					name, _ = o["name"].(string)
				}
			}
			results = append(results, map[string]any{
				"result": result, "policy": m["policy"], "rule": m["rule"],
				"message": m["message"], "severity": m["severity"],
				"kind": kind, "name": name, "namespace": ns,
			})
		}
	}
	rank := map[string]int{"fail": 0, "error": 1, "warn": 2}
	sort.SliceStable(results, func(i, j int) bool {
		return rank[fmt.Sprint(results[i]["result"])] < rank[fmt.Sprint(results[j]["result"])]
	})
	truncated := false
	if len(results) > maxReportResults {
		results, truncated = results[:maxReportResults], true
	}
	return map[string]any{
		"namespace": ns, "totals": counts, "results": results, "truncated": truncated,
	}, nil
}

func strSlice(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
