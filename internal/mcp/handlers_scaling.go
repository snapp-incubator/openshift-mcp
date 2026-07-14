package mcp

import (
	"context"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

func handleListHPAs(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	list, err := c.Clientset.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{
		Limit: defaultListLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list horizontalpodautoscalers in %s: %w", ns, err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		h := &list.Items[i]
		minReplicas := int32(1)
		if h.Spec.MinReplicas != nil {
			minReplicas = *h.Spec.MinReplicas
		}
		entry := map[string]any{
			"name": h.Name, "namespace": ns,
			"target":  fmt.Sprintf("%s/%s", h.Spec.ScaleTargetRef.Kind, h.Spec.ScaleTargetRef.Name),
			"min":     minReplicas,
			"max":     h.Spec.MaxReplicas,
			"current": h.Status.CurrentReplicas,
			"desired": h.Status.DesiredReplicas,
			"metrics": hpaMetrics(h),
			"age":     age(h.CreationTimestamp.Time),
		}
		if problems := hpaProblems(h); len(problems) > 0 {
			entry["problems"] = problems
		}
		if h.Status.CurrentReplicas >= h.Spec.MaxReplicas && h.Spec.MaxReplicas > 0 {
			entry["at_max"] = true
		}
		out = append(out, entry)
	}
	return map[string]any{"namespace": ns, "count": len(out), "hpas": out}, nil
}

func hpaMetrics(h *autoscalingv2.HorizontalPodAutoscaler) []string {
	out := make([]string, 0, len(h.Spec.Metrics))
	for i, spec := range h.Spec.Metrics {
		name, target := metricSpecText(spec)
		current := "<unknown>"
		if i < len(h.Status.CurrentMetrics) {
			if v := metricStatusText(h.Status.CurrentMetrics[i]); v != "" {
				current = v
			}
		}
		out = append(out, fmt.Sprintf("%s: current %s, target %s", name, current, target))
	}
	return out
}

func metricSpecText(m autoscalingv2.MetricSpec) (name, target string) {
	switch m.Type {
	case autoscalingv2.ResourceMetricSourceType:
		if m.Resource == nil {
			return "resource", "<none>"
		}
		return string(m.Resource.Name), targetText(m.Resource.Target)
	case autoscalingv2.PodsMetricSourceType:
		if m.Pods == nil {
			return "pods", "<none>"
		}
		return "pods/" + m.Pods.Metric.Name, targetText(m.Pods.Target)
	case autoscalingv2.ObjectMetricSourceType:
		if m.Object == nil {
			return "object", "<none>"
		}
		return "object/" + m.Object.Metric.Name, targetText(m.Object.Target)
	case autoscalingv2.ExternalMetricSourceType:
		if m.External == nil {
			return "external", "<none>"
		}
		return "external/" + m.External.Metric.Name, targetText(m.External.Target)
	case autoscalingv2.ContainerResourceMetricSourceType:
		if m.ContainerResource == nil {
			return "containerResource", "<none>"
		}
		return fmt.Sprintf("%s[%s]", m.ContainerResource.Name, m.ContainerResource.Container),
			targetText(m.ContainerResource.Target)
	}
	return string(m.Type), "<none>"
}

func targetText(t autoscalingv2.MetricTarget) string {
	switch {
	case t.AverageUtilization != nil:
		return fmt.Sprintf("%d%%", *t.AverageUtilization)
	case t.AverageValue != nil:
		return t.AverageValue.String()
	case t.Value != nil:
		return t.Value.String()
	}
	return "<none>"
}

func metricStatusText(m autoscalingv2.MetricStatus) string {
	switch m.Type {
	case autoscalingv2.ResourceMetricSourceType:
		if m.Resource != nil {
			return currentText(m.Resource.Current)
		}
	case autoscalingv2.PodsMetricSourceType:
		if m.Pods != nil {
			return currentText(m.Pods.Current)
		}
	case autoscalingv2.ObjectMetricSourceType:
		if m.Object != nil {
			return currentText(m.Object.Current)
		}
	case autoscalingv2.ExternalMetricSourceType:
		if m.External != nil {
			return currentText(m.External.Current)
		}
	case autoscalingv2.ContainerResourceMetricSourceType:
		if m.ContainerResource != nil {
			return currentText(m.ContainerResource.Current)
		}
	}
	return ""
}

func currentText(v autoscalingv2.MetricValueStatus) string {
	switch {
	case v.AverageUtilization != nil:
		return fmt.Sprintf("%d%%", *v.AverageUtilization)
	case v.AverageValue != nil:
		return v.AverageValue.String()
	case v.Value != nil:
		return v.Value.String()
	}
	return ""
}

func hpaProblems(h *autoscalingv2.HorizontalPodAutoscaler) []string {
	var out []string
	for _, cond := range h.Status.Conditions {
		bad := (cond.Type == autoscalingv2.ScalingActive && cond.Status == corev1.ConditionFalse) ||
			(cond.Type == autoscalingv2.AbleToScale && cond.Status == corev1.ConditionFalse) ||
			(cond.Type == autoscalingv2.ScalingLimited && cond.Status == corev1.ConditionTrue)
		if bad {
			out = append(out, fmt.Sprintf("%s=%s (%s): %s", cond.Type, cond.Status, cond.Reason, cond.Message))
		}
	}
	return out
}

func handleListPDBs(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	list, err := c.Clientset.PolicyV1().PodDisruptionBudgets(ns).List(ctx, metav1.ListOptions{
		Limit: defaultListLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list poddisruptionbudgets in %s: %w", ns, err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		entry := map[string]any{
			"name": p.Name, "namespace": ns,
			"applies_to":          selectorText(p.Spec.Selector),
			"current_healthy":     p.Status.CurrentHealthy,
			"desired_healthy":     p.Status.DesiredHealthy,
			"expected_pods":       p.Status.ExpectedPods,
			"disruptions_allowed": p.Status.DisruptionsAllowed,
			"age":                 age(p.CreationTimestamp.Time),
		}
		if p.Spec.MinAvailable != nil {
			entry["min_available"] = p.Spec.MinAvailable.String()
		}
		if p.Spec.MaxUnavailable != nil {
			entry["max_unavailable"] = p.Spec.MaxUnavailable.String()
		}
		if p.Status.DisruptionsAllowed == 0 {
			entry["blocking"] = "0 disruptions allowed: this PDB blocks node drains and rolling upgrades"
		}
		out = append(out, entry)
	}
	return map[string]any{"namespace": ns, "count": len(out), "pdbs": out}, nil
}
