package mcp

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

func jobSummaries(ctx context.Context, c *k8s.Client, ns string) ([]map[string]any, error) {
	list, err := c.Clientset.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{Limit: defaultListLimit})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		j := &list.Items[i]
		completions := int32(1)
		if j.Spec.Completions != nil {
			completions = *j.Spec.Completions
		}
		entry := map[string]any{
			"kind": "Job", "name": j.Name, "namespace": ns,
			"ready":  fmt.Sprintf("%d/%d", j.Status.Succeeded, completions),
			"active": j.Status.Active,
			"failed": j.Status.Failed,
			"age":    age(j.CreationTimestamp.Time),
		}
		if reason := jobProblem(j); reason != "" {
			entry["degraded_reason"] = reason
		}
		out = append(out, entry)
	}
	return out, nil
}

func jobProblem(j *batchv1.Job) string {
	for _, cond := range j.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return fmt.Sprintf("%s: %s", cond.Reason, cond.Message)
		}
	}
	if j.Status.Failed > 0 {
		backoff := int32(6)
		if j.Spec.BackoffLimit != nil {
			backoff = *j.Spec.BackoffLimit
		}
		return fmt.Sprintf("%d pod failure(s), backoffLimit %d", j.Status.Failed, backoff)
	}
	return ""
}

func cronJobSummaries(ctx context.Context, c *k8s.Client, ns string) ([]map[string]any, error) {
	list, err := c.Clientset.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{Limit: defaultListLimit})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		cj := &list.Items[i]
		entry := map[string]any{
			"kind": "CronJob", "name": cj.Name, "namespace": ns,
			"schedule": cj.Spec.Schedule,
			"active":   len(cj.Status.Active),
			"age":      age(cj.CreationTimestamp.Time),
		}
		if cj.Status.LastScheduleTime != nil {
			entry["last_schedule"] = age(cj.Status.LastScheduleTime.Time) + " ago"
		} else {
			entry["last_schedule"] = "never"
		}
		if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
			entry["degraded_reason"] = "suspended: this CronJob will not run until spec.suspend is false"
		}
		out = append(out, entry)
	}
	return out, nil
}

func jobDetail(ctx context.Context, c *k8s.Client, ns, name string) (*metav1.LabelSelector, map[string]any, []map[string]any, error) {
	j, err := c.Clientset.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get job %s/%s: %w", ns, name, err)
	}
	status := map[string]any{
		"active": j.Status.Active, "succeeded": j.Status.Succeeded, "failed": j.Status.Failed,
	}
	if j.Spec.Completions != nil {
		status["completions"] = *j.Spec.Completions
	}
	if j.Spec.Parallelism != nil {
		status["parallelism"] = *j.Spec.Parallelism
	}
	if j.Spec.BackoffLimit != nil {
		status["backoff_limit"] = *j.Spec.BackoffLimit
	}
	if j.Status.StartTime != nil {
		status["started"] = age(j.Status.StartTime.Time) + " ago"
	}
	if j.Status.CompletionTime != nil {
		status["completed"] = age(j.Status.CompletionTime.Time) + " ago"
	}
	var conditions []map[string]any
	for _, cond := range j.Status.Conditions {
		conditions = append(conditions, map[string]any{
			"type": string(cond.Type), "status": string(cond.Status),
			"reason": cond.Reason, "message": cond.Message,
		})
	}
	return j.Spec.Selector, status, conditions, nil
}

func cronJobDetail(ctx context.Context, c *k8s.Client, ns, name string) (*metav1.LabelSelector, map[string]any, []map[string]any, error) {
	cj, err := c.Clientset.BatchV1().CronJobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get cronjob %s/%s: %w", ns, name, err)
	}
	activeJobs := make([]string, 0, len(cj.Status.Active))
	for _, ref := range cj.Status.Active {
		activeJobs = append(activeJobs, ref.Name)
	}
	status := map[string]any{
		"schedule":           cj.Spec.Schedule,
		"suspended":          cj.Spec.Suspend != nil && *cj.Spec.Suspend,
		"concurrency_policy": string(cj.Spec.ConcurrencyPolicy),
		"active_jobs":        activeJobs,
		"hint":               "Inspect a listed active job with get_workload kind=job.",
	}
	if cj.Spec.StartingDeadlineSeconds != nil {
		status["starting_deadline_seconds"] = *cj.Spec.StartingDeadlineSeconds
	}
	if cj.Status.LastScheduleTime != nil {
		status["last_schedule"] = age(cj.Status.LastScheduleTime.Time) + " ago"
	} else {
		status["last_schedule"] = "never"
	}
	if cj.Status.LastSuccessfulTime != nil {
		status["last_success"] = age(cj.Status.LastSuccessfulTime.Time) + " ago"
	}
	return nil, status, nil, nil
}

func replicaSetDetail(ctx context.Context, c *k8s.Client, ns, name string) (*metav1.LabelSelector, map[string]any, []map[string]any, error) {
	rs, err := c.Clientset.AppsV1().ReplicaSets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get replicaset %s/%s: %w", ns, name, err)
	}
	desired := int32(1)
	if rs.Spec.Replicas != nil {
		desired = *rs.Spec.Replicas
	}
	status := map[string]any{
		"desired": desired, "ready": rs.Status.ReadyReplicas,
		"available": rs.Status.AvailableReplicas, "current": rs.Status.Replicas,
	}
	if len(rs.OwnerReferences) > 0 {
		status["owned_by"] = fmt.Sprintf("%s/%s", rs.OwnerReferences[0].Kind, rs.OwnerReferences[0].Name)
	}
	var conditions []map[string]any
	for _, cond := range rs.Status.Conditions {
		conditions = append(conditions, map[string]any{
			"type": string(cond.Type), "status": string(cond.Status),
			"reason": cond.Reason, "message": cond.Message,
		})
	}
	return rs.Spec.Selector, status, conditions, nil
}
