package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

func handleListNamespaces(ctx context.Context, c *k8s.Client, a args) (any, error) {
	list, err := c.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: a.str("selector"),
	})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	filter := strings.ToLower(a.str("filter"))
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		ns := &list.Items[i]
		if filter != "" && !strings.Contains(strings.ToLower(ns.Name), filter) {
			continue
		}
		entry := map[string]any{
			"name":  ns.Name,
			"phase": string(ns.Status.Phase),
			"age":   age(ns.CreationTimestamp.Time),
		}
		if ns.Status.Phase == corev1.NamespaceTerminating {
			entry["note"] = "stuck Terminating usually means a resource finalizer is not completing"
		}
		out = append(out, entry)
	}
	return map[string]any{"count": len(out), "namespaces": out}, nil
}

func handleAPIResources(ctx context.Context, c *k8s.Client, a args) (any, error) {
	lists, err := c.Clientset.Discovery().ServerPreferredResources()
	if err != nil && len(lists) == 0 {
		return nil, fmt.Errorf("discover api resources: %w", err)
	}

	filter := strings.ToLower(a.str("filter"))
	wantGroup := a.str("group")
	groupFiltered := wantGroup != ""

	type apiRes struct {
		Group      string   `json:"group"`
		Version    string   `json:"version"`
		Resource   string   `json:"resource"`
		Kind       string   `json:"kind"`
		Namespaced bool     `json:"namespaced"`
		Verbs      []string `json:"verbs"`
	}
	var out []apiRes
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv, gvErr := schema.ParseGroupVersion(list.GroupVersion)
		if gvErr != nil {
			continue
		}
		if groupFiltered && gv.Group != wantGroup {
			continue
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") {
				continue
			}
			if !hasVerb(r.Verbs, "get") && !hasVerb(r.Verbs, "list") {
				continue
			}
			if filter != "" && !matchesResource(r.Name, r.Kind, gv.Group, filter) {
				continue
			}
			out = append(out, apiRes{
				Group: gv.Group, Version: gv.Version, Resource: r.Name,
				Kind: r.Kind, Namespaced: r.Namespaced, Verbs: r.Verbs,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Resource < out[j].Resource
	})

	res := map[string]any{
		"count": len(out), "resources": out,
		"note": "Pass group/version/resource to get_resource or list_resource. Empty group means core (v1).",
	}
	if err != nil {
		res["warning"] = fmt.Sprintf("some API groups did not respond and are omitted: %v", err)
	}
	return res, nil
}

func matchesResource(name, kind, group, filter string) bool {
	return strings.Contains(strings.ToLower(name), filter) ||
		strings.Contains(strings.ToLower(kind), filter) ||
		strings.Contains(strings.ToLower(group), filter)
}

func hasVerb(verbs metav1.Verbs, want string) bool {
	for _, v := range verbs {
		if v == want {
			return true
		}
	}
	return false
}

var nodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}

type nodeUsage struct {
	cpuMilli int64
	memoryMi int64
}

func (u nodeUsage) render(allocatable corev1.ResourceList) map[string]any {
	out := map[string]any{
		"cpu_millicores": u.cpuMilli,
		"memory_mi":      u.memoryMi,
	}
	if cpu := allocatable.Cpu().MilliValue(); cpu > 0 {
		out["cpu_percent"] = fmt.Sprintf("%.0f%%", float64(u.cpuMilli)/float64(cpu)*100)
	}
	if mem := allocatable.Memory().Value() / (1024 * 1024); mem > 0 {
		out["memory_percent"] = fmt.Sprintf("%.0f%%", float64(u.memoryMi)/float64(mem)*100)
	}
	return out
}

func nodeUsageByName(ctx context.Context, c *k8s.Client) (map[string]nodeUsage, error) {
	if c.Dynamic == nil {
		return nil, fmt.Errorf("no dynamic client")
	}
	list, err := c.Dynamic.Resource(nodeMetricsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("node metrics (metrics-server/metrics.k8s.io required): %w", err)
	}
	out := make(map[string]nodeUsage, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		var u nodeUsage
		if s, found, _ := unstructured.NestedString(item.Object, "usage", "cpu"); found {
			if q, qerr := resource.ParseQuantity(s); qerr == nil {
				u.cpuMilli = q.MilliValue()
			}
		}
		if s, found, _ := unstructured.NestedString(item.Object, "usage", "memory"); found {
			if q, qerr := resource.ParseQuantity(s); qerr == nil {
				u.memoryMi = q.Value() / (1024 * 1024)
			}
		}
		out[item.GetName()] = u
	}
	return out, nil
}
