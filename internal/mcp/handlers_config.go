package mcp

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

func handleListConfig(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	selector := a.str("selector")

	configMaps, cmErr := configMapSummaries(ctx, c, ns, selector)
	secrets, secErr := secretSummaries(ctx, c, ns, selector)

	if cmErr != nil && secErr != nil {
		return nil, fmt.Errorf("list config in %s: %w", ns, cmErr)
	}

	res := map[string]any{
		"namespace":  ns,
		"configmaps": configMaps,
		"secrets":    secrets,
		"note": "Key names only. Secret values are never exposed; ConfigMap values are available via " +
			"get_resource (version=v1, resource=configmaps). A missing name or key here explains CreateContainerConfigError.",
	}
	var warnings []string
	if cmErr != nil {
		warnings = append(warnings, fmt.Sprintf("configmaps not listed: %v", cmErr))
	}
	if secErr != nil {
		warnings = append(warnings, fmt.Sprintf("secrets not listed: %v", secErr))
	}
	if warnings != nil {
		res["warnings"] = warnings
	}
	return res, nil
}

func configMapSummaries(ctx context.Context, c *k8s.Client, ns, selector string) ([]map[string]any, error) {
	list, err := c.Clientset.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
		Limit:         defaultListLimit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		cm := &list.Items[i]
		keys := make([]string, 0, len(cm.Data)+len(cm.BinaryData))
		bytes := 0
		for k, v := range cm.Data {
			keys = append(keys, k)
			bytes += len(v)
		}
		for k, v := range cm.BinaryData {
			keys = append(keys, k+" (binary)")
			bytes += len(v)
		}
		sort.Strings(keys)
		out = append(out, map[string]any{
			"name": cm.Name, "namespace": ns,
			"keys": keys, "total_bytes": bytes,
			"age": age(cm.CreationTimestamp.Time),
		})
	}
	return out, nil
}

func secretSummaries(ctx context.Context, c *k8s.Client, ns, selector string) ([]map[string]any, error) {
	list, err := c.Clientset.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
		Limit:         defaultListLimit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		sec := &list.Items[i]
		keys := make([]string, 0, len(sec.Data))
		for k := range sec.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out = append(out, map[string]any{
			"name": sec.Name, "namespace": ns,
			"type": string(sec.Type), "keys": keys,
			"age": age(sec.CreationTimestamp.Time),
		})
	}
	return out, nil
}
