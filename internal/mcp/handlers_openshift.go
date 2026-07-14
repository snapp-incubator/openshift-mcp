package mcp

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

var (
	clusterOperatorGVR  = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "clusteroperators"}
	clusterVersionGVR   = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "clusterversions"}
	buildGVR            = schema.GroupVersionResource{Group: "build.openshift.io", Version: "v1", Resource: "builds"}
	imageStreamGVR      = schema.GroupVersionResource{Group: "image.openshift.io", Version: "v1", Resource: "imagestreams"}
	deploymentConfigGVR = schema.GroupVersionResource{Group: "apps.openshift.io", Version: "v1", Resource: "deploymentconfigs"}
	machineGVR          = schema.GroupVersionResource{Group: "machine.openshift.io", Version: "v1beta1", Resource: "machines"}
)

const machineAPINamespace = "openshift-machine-api"

func handleListClusterOperators(ctx context.Context, c *k8s.Client, a args) (any, error) {
	list, err := c.Dynamic.Resource(clusterOperatorGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list clusteroperators (OpenShift config API required): %w", err)
	}
	onlyUnhealthy := a.boolean("unhealthy_only")

	out := make([]map[string]any, 0, len(list.Items))
	degraded := 0
	for i := range list.Items {
		co := &list.Items[i]
		conds := conditionMap(co)
		available := conds["Available"]
		isDegraded := conds["Degraded"].status == "True"
		notAvailable := available.status != "True"
		if isDegraded || notAvailable {
			degraded++
		}
		if onlyUnhealthy && !isDegraded && !notAvailable {
			continue
		}
		entry := map[string]any{
			"name":        co.GetName(),
			"available":   available.status,
			"degraded":    conds["Degraded"].status,
			"progressing": conds["Progressing"].status,
			"version":     operatorVersion(co),
		}
		if isDegraded && conds["Degraded"].message != "" {
			entry["degraded_message"] = conds["Degraded"].message
		}
		if notAvailable && available.message != "" {
			entry["unavailable_message"] = available.message
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["name"]) < fmt.Sprint(out[j]["name"])
	})
	return map[string]any{
		"count": len(out), "unhealthy_total": degraded, "cluster_operators": out,
	}, nil
}

func operatorVersion(co *unstructured.Unstructured) string {
	versions, _, _ := unstructured.NestedSlice(co.Object, "status", "versions")
	for _, raw := range versions {
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if v["name"] == "operator" {
			return fmt.Sprint(v["version"])
		}
	}
	return ""
}

func handleGetClusterVersion(ctx context.Context, c *k8s.Client, _ args) (any, error) {
	cv, err := c.Dynamic.Resource(clusterVersionGVR).Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get clusterversion (OpenShift config API required): %w", err)
	}
	version, _, _ := unstructured.NestedString(cv.Object, "status", "desired", "version")
	channel, _, _ := unstructured.NestedString(cv.Object, "spec", "channel")
	clusterID, _, _ := unstructured.NestedString(cv.Object, "spec", "clusterID")

	conds := conditionMap(cv)
	out := map[string]any{
		"version": version, "channel": channel, "cluster_id": clusterID,
		"available":   conds["Available"].status,
		"progressing": conds["Progressing"].status,
		"degraded":    conds["Failing"].status,
	}
	if msg := conds["Progressing"].message; msg != "" {
		out["progressing_message"] = msg
	}
	if conds["Failing"].status == "True" {
		out["failing_message"] = conds["Failing"].message
	}

	updates, _, _ := unstructured.NestedSlice(cv.Object, "status", "availableUpdates")
	var available []string
	for _, raw := range updates {
		if u, ok := raw.(map[string]any); ok {
			available = append(available, fmt.Sprint(u["version"]))
		}
	}
	out["available_updates"] = available

	history, _, _ := unstructured.NestedSlice(cv.Object, "status", "history")
	var recent []map[string]any
	for i, raw := range history {
		if i >= 3 {
			break
		}
		if h, ok := raw.(map[string]any); ok {
			recent = append(recent, map[string]any{
				"version": h["version"], "state": h["state"], "started": h["startedTime"],
			})
		}
	}
	out["recent_history"] = recent
	return out, nil
}

func handleListBuilds(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	list, err := c.Dynamic.Resource(buildGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: a.str("selector"),
		Limit:         defaultListLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list builds in %s (OpenShift Build API required): %w", ns, err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		b := &list.Items[i]
		phase, _, _ := unstructured.NestedString(b.Object, "status", "phase")
		entry := map[string]any{
			"name": b.GetName(), "namespace": ns, "phase": phase,
			"age": age(b.GetCreationTimestamp().Time),
		}
		if strategy, found, _ := unstructured.NestedString(b.Object, "spec", "strategy", "type"); found {
			entry["strategy"] = strategy
		}
		if reason, found, _ := unstructured.NestedString(b.Object, "status", "reason"); found {
			entry["reason"] = reason
		}
		if msg, found, _ := unstructured.NestedString(b.Object, "status", "message"); found {
			entry["message"] = msg
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["name"]) < fmt.Sprint(out[j]["name"])
	})
	return map[string]any{
		"namespace": ns, "count": len(out), "builds": out,
		"note": "Build logs come from the build's pod: pod_logs with name '<build-name>-build'.",
	}, nil
}

func handleListImageStreams(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	list, err := c.Dynamic.Resource(imageStreamGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		Limit: defaultListLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list imagestreams in %s (OpenShift Image API required): %w", ns, err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		is := &list.Items[i]
		repo, _, _ := unstructured.NestedString(is.Object, "status", "dockerImageRepository")
		entry := map[string]any{
			"name": is.GetName(), "namespace": ns, "repository": repo,
			"age": age(is.GetCreationTimestamp().Time),
		}
		tags, _, _ := unstructured.NestedSlice(is.Object, "status", "tags")
		var tagNames []string
		var emptyTags []string
		for _, raw := range tags {
			t, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name := fmt.Sprint(t["tag"])
			items, _, _ := unstructured.NestedSlice(t, "items")
			if len(items) == 0 {
				emptyTags = append(emptyTags, name)
				continue
			}
			tagNames = append(tagNames, name)
		}
		entry["tags"] = tagNames
		if len(emptyTags) > 0 {
			entry["unresolved_tags"] = emptyTags
			entry["warning"] = "listed tags resolve to no image; pods referencing them cannot pull"
		}
		out = append(out, entry)
	}
	return map[string]any{"namespace": ns, "count": len(out), "image_streams": out}, nil
}

func deploymentConfigSummaries(ctx context.Context, c *k8s.Client, ns string) ([]map[string]any, error) {
	if c.Dynamic == nil {
		return nil, nil
	}
	list, err := c.Dynamic.Resource(deploymentConfigGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		Limit: defaultListLimit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		dc := &list.Items[i]
		desired, _, _ := unstructured.NestedInt64(dc.Object, "spec", "replicas")
		ready, _, _ := unstructured.NestedInt64(dc.Object, "status", "readyReplicas")
		latest, _, _ := unstructured.NestedInt64(dc.Object, "status", "latestVersion")
		entry := map[string]any{
			"kind": "DeploymentConfig", "name": dc.GetName(), "namespace": ns,
			"ready":          fmt.Sprintf("%d/%d", ready, desired),
			"latest_version": latest,
			"age":            age(dc.GetCreationTimestamp().Time),
		}
		if ready < desired {
			conds := conditionMap(dc)
			if cond := conds["Available"]; cond.status != "True" && cond.message != "" {
				entry["degraded_reason"] = cond.message
			} else if cond := conds["Progressing"]; cond.status == "False" && cond.message != "" {
				entry["degraded_reason"] = fmt.Sprintf("%s: %s", cond.reason, cond.message)
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

func handleListMachines(ctx context.Context, c *k8s.Client, a args) (any, error) {
	list, err := c.Dynamic.Resource(machineGVR).Namespace(machineAPINamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list machines (OpenShift Machine API required): %w", err)
	}
	onlyUnhealthy := a.boolean("unhealthy_only")

	out := make([]map[string]any, 0, len(list.Items))
	unhealthy := 0
	for i := range list.Items {
		m := &list.Items[i]
		phase, _, _ := unstructured.NestedString(m.Object, "status", "phase")
		bad := phase != "Running"
		if bad {
			unhealthy++
		}
		if onlyUnhealthy && !bad {
			continue
		}
		entry := map[string]any{
			"name": m.GetName(), "phase": phase,
			"age": age(m.GetCreationTimestamp().Time),
		}
		if node, found, _ := unstructured.NestedString(m.Object, "status", "nodeRef", "name"); found {
			entry["node"] = node
		} else if phase == "Running" {
			entry["warning"] = "Running but no node registered: the kubelet never joined the cluster"
		}
		if role, found := m.GetLabels()["machine.openshift.io/cluster-api-machine-role"]; found {
			entry["role"] = role
		}
		if msg, found, _ := unstructured.NestedString(m.Object, "status", "errorMessage"); found {
			entry["error"] = msg
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["name"]) < fmt.Sprint(out[j]["name"])
	})
	return map[string]any{
		"count": len(out), "not_running_total": unhealthy, "machines": out,
	}, nil
}

type condInfo struct {
	status  string
	reason  string
	message string
}

func conditionMap(obj *unstructured.Unstructured) map[string]condInfo {
	out := map[string]condInfo{}
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conds {
		cond, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := cond["type"].(string)
		if typ == "" {
			continue
		}
		info := condInfo{}
		if v, ok := cond["status"].(string); ok {
			info.status = v
		}
		if v, ok := cond["reason"].(string); ok {
			info.reason = v
		}
		if v, ok := cond["message"].(string); ok {
			info.message = v
		}
		out[typ] = info
	}
	return out
}
