package mcp

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

const (
	sevCritical = 3
	sevWarning  = 2
	sevInfo     = 1
)

type finding struct {
	severity int
	Level    string `json:"level"`
	Object   string `json:"object"`
	Problem  string `json:"problem"`
	NextStep string `json:"next_step,omitempty"`
}

func handleDiagnoseNamespace(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	var findings []finding
	var skipped []string

	pods, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{Limit: defaultListLimit})
	if err != nil {
		return nil, fmt.Errorf("diagnose %s: list pods: %w", ns, err)
	}
	findings = append(findings, podFindings(pods)...)

	findings = appendOrSkip(findings, &skipped, "workloads", func() ([]finding, error) {
		return workloadFindings(ctx, c, ns)
	})
	findings = appendOrSkip(findings, &skipped, "services", func() ([]finding, error) {
		return serviceFindings(ctx, c, ns)
	})
	findings = appendOrSkip(findings, &skipped, "quota", func() ([]finding, error) {
		return quotaFindings(ctx, c, ns)
	})
	findings = appendOrSkip(findings, &skipped, "pvcs", func() ([]finding, error) {
		return pvcFindings(ctx, c, ns)
	})

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].severity != findings[j].severity {
			return findings[i].severity > findings[j].severity
		}
		return findings[i].Object < findings[j].Object
	})

	out := map[string]any{
		"namespace": ns,
		"pod_count": len(pods.Items),
		"findings":  findings,
	}
	if len(findings) == 0 {
		out["summary"] = "No problems detected in the checks that ran."
	} else {
		out["summary"] = fmt.Sprintf("%d problem(s) found; most severe first.", len(findings))
	}
	if len(skipped) > 0 {
		out["skipped_checks"] = skipped
		out["skipped_note"] = "These checks could not run (usually missing RBAC). Their absence is not a clean bill of health."
	}
	return out, nil
}

func appendOrSkip(dst []finding, skipped *[]string, name string, check func() ([]finding, error)) []finding {
	got, err := check()
	if err != nil {
		*skipped = append(*skipped, fmt.Sprintf("%s: %v", name, err))
		return dst
	}
	return append(dst, got...)
}

func podFindings(pods *corev1.PodList) []finding {
	var out []finding
	for i := range pods.Items {
		pod := &pods.Items[i]
		obj := "pod/" + pod.Name

		if pod.Status.Phase == corev1.PodPending {
			out = append(out, finding{
				severity: sevCritical, Level: "critical", Object: obj,
				Problem:  "Pending: not scheduled onto a node",
				NextStep: "get_pod for the FailedScheduling event; then get_quota and list_nodes",
			})
			continue
		}

		for _, st := range pod.Status.ContainerStatuses {
			if st.State.Waiting != nil {
				reason := st.State.Waiting.Reason
				switch reason {
				case "CrashLoopBackOff":
					out = append(out, finding{
						severity: sevCritical, Level: "critical", Object: obj,
						Problem:  fmt.Sprintf("container %q crash-looping (%d restarts)", st.Name, st.RestartCount),
						NextStep: fmt.Sprintf("pod_logs name=%s container=%s previous=true", pod.Name, st.Name),
					})
				case "ImagePullBackOff", "ErrImagePull":
					out = append(out, finding{
						severity: sevCritical, Level: "critical", Object: obj,
						Problem:  fmt.Sprintf("container %q cannot pull image: %s", st.Name, reason),
						NextStep: "check the image tag and pull secret; on OpenShift also list_builds and list_imagestreams",
					})
				case "CreateContainerConfigError":
					out = append(out, finding{
						severity: sevCritical, Level: "critical", Object: obj,
						Problem:  fmt.Sprintf("container %q config error: %s", st.Name, st.State.Waiting.Message),
						NextStep: "list_configmaps and list_secrets — a referenced object or key is missing",
					})
				case "", "ContainerCreating", "PodInitializing":
				default:
					out = append(out, finding{
						severity: sevWarning, Level: "warning", Object: obj,
						Problem:  fmt.Sprintf("container %q waiting: %s", st.Name, reason),
						NextStep: "get_pod name=" + pod.Name,
					})
				}
				continue
			}
			if last := st.LastTerminationState.Terminated; last != nil && last.Reason == "OOMKilled" {
				out = append(out, finding{
					severity: sevWarning, Level: "warning", Object: obj,
					Problem:  fmt.Sprintf("container %q was OOMKilled", st.Name),
					NextStep: "compare top_pods usage against the memory limit in get_pod",
				})
			}
		}
	}
	return out
}

func workloadFindings(ctx context.Context, c *k8s.Client, ns string) ([]finding, error) {
	var out []finding
	deps, err := c.Clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		if d.Status.ReadyReplicas >= desired {
			continue
		}
		level, sev := "warning", sevWarning
		if d.Status.ReadyReplicas == 0 && desired > 0 {
			level, sev = "critical", sevCritical
		}
		problem := fmt.Sprintf("deployment %d/%d replicas ready", d.Status.ReadyReplicas, desired)
		if reason := deploymentProblem(d.Status.Conditions); reason != "" {
			problem += " — " + reason
		}
		out = append(out, finding{
			severity: sev, Level: level, Object: "deployment/" + d.Name,
			Problem:  problem,
			NextStep: fmt.Sprintf("get_workload kind=deployment name=%s", d.Name),
		})
	}
	return out, nil
}

func serviceFindings(ctx context.Context, c *k8s.Client, ns string) ([]finding, error) {
	var out []finding
	svcs, err := c.Clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range svcs.Items {
		svc := &svcs.Items[i]
		if svc.Spec.Type == corev1.ServiceTypeExternalName || len(svc.Spec.Selector) == 0 {
			continue
		}
		ready, notReady := endpointCounts(ctx, c, ns, svc.Name)
		if ready > 0 {
			continue
		}
		problem := "service has 0 ready endpoints — nothing is serving it"
		if notReady > 0 {
			problem = fmt.Sprintf("service has 0 ready endpoints (%d not-ready) — backends exist but fail readiness", notReady)
		}
		out = append(out, finding{
			severity: sevCritical, Level: "critical", Object: "service/" + svc.Name,
			Problem:  problem,
			NextStep: fmt.Sprintf("get_service name=%s to compare its selector against pod labels", svc.Name),
		})
	}
	return out, nil
}

func quotaFindings(ctx context.Context, c *k8s.Client, ns string) ([]finding, error) {
	var out []finding
	quotas, err := c.Clientset.CoreV1().ResourceQuotas(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range quotas.Items {
		q := &quotas.Items[i]
		for res, hard := range q.Status.Hard {
			used, ok := q.Status.Used[res]
			if !ok {
				continue
			}
			if used.Cmp(hard) < 0 {
				continue
			}
			out = append(out, finding{
				severity: sevCritical, Level: "critical", Object: "resourcequota/" + q.Name,
				Problem: fmt.Sprintf("%s exhausted: %s of %s used — new pods will be rejected",
					res, used.String(), hard.String()),
				NextStep: "get_quota for the full picture; reduce requests or raise the quota",
			})
		}
	}
	return out, nil
}

func pvcFindings(ctx context.Context, c *k8s.Client, ns string) ([]finding, error) {
	var out []finding
	pvcs, err := c.Clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if pvc.Status.Phase == corev1.ClaimBound {
			continue
		}
		out = append(out, finding{
			severity: sevCritical, Level: "critical", Object: "pvc/" + pvc.Name,
			Problem:  fmt.Sprintf("PVC is %s, not Bound — pods mounting it stay Pending", pvc.Status.Phase),
			NextStep: "list_pvcs; check the storage class exists and has available capacity",
		})
	}
	return out, nil
}
