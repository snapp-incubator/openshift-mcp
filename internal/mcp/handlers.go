package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"gitlab.snapp.ir/snappcloud/openshift-mcp/internal/k8s"
)

const defaultListLimit = 100

// --- Pods ---

func handleListPods(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	limit := a.intOr("limit", defaultListLimit)
	if limit < 1 || limit > 500 {
		limit = defaultListLimit
	}
	list, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: a.str("selector"),
		FieldSelector: a.str("field_selector"),
		Limit:         int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", ns, err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, podSummary(&list.Items[i]))
	}
	return map[string]any{"namespace": ns, "count": len(out), "pods": out}, nil
}

func handleGetPod(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns, name := a.str("namespace"), a.str("name")
	pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod %s/%s: %w", ns, name, err)
	}

	containers := make([]map[string]any, 0, len(pod.Spec.Containers))
	for _, ct := range pod.Spec.Containers {
		entry := map[string]any{
			"name":  ct.Name,
			"image": ct.Image,
		}
		if len(ct.Resources.Requests) > 0 || len(ct.Resources.Limits) > 0 {
			entry["resources"] = map[string]any{
				"requests": quantities(ct.Resources.Requests),
				"limits":   quantities(ct.Resources.Limits),
			}
		}
		for _, st := range pod.Status.ContainerStatuses {
			if st.Name != ct.Name {
				continue
			}
			entry["ready"] = st.Ready
			entry["restarts"] = st.RestartCount
			entry["state"] = containerState(st.State)
			if last := containerState(st.LastTerminationState); last != "" {
				entry["last_state"] = last
			}
		}
		containers = append(containers, entry)
	}

	conditions := make([]map[string]any, 0, len(pod.Status.Conditions))
	for _, cond := range pod.Status.Conditions {
		entry := map[string]any{"type": string(cond.Type), "status": string(cond.Status)}
		if cond.Reason != "" {
			entry["reason"] = cond.Reason
		}
		if cond.Message != "" {
			entry["message"] = cond.Message
		}
		conditions = append(conditions, entry)
	}

	events, _ := objectEvents(ctx, c, ns, "Pod", name, 10)

	return map[string]any{
		"namespace":  ns,
		"name":       name,
		"phase":      string(pod.Status.Phase),
		"node":       pod.Spec.NodeName,
		"pod_ip":     pod.Status.PodIP,
		"qos_class":  string(pod.Status.QOSClass),
		"labels":     pod.Labels,
		"age":        age(pod.CreationTimestamp.Time),
		"containers": containers,
		"conditions": conditions,
		"events":     events,
	}, nil
}

func handlePodLogs(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns, name := a.str("namespace"), a.str("name")
	tail := int64(a.intOr("tail_lines", 100))
	if tail > 2000 {
		tail = 2000
	}
	opts := &corev1.PodLogOptions{
		Container: a.str("container"),
		TailLines: &tail,
		Previous:  a.boolean("previous"),
	}
	raw, err := c.Clientset.CoreV1().Pods(ns).GetLogs(name, opts).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("logs for %s/%s: %w", ns, name, err)
	}
	if len(raw) == 0 {
		return "(no log output)", nil
	}
	// Cap the payload so a chatty container cannot blow up the context.
	const maxBytes = 64 * 1024
	if len(raw) > maxBytes {
		raw = raw[len(raw)-maxBytes:]
	}
	return string(raw), nil
}

// --- Events ---

func handleListEvents(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	var fields []string
	if obj := a.str("object"); obj != "" {
		fields = append(fields, "involvedObject.name="+obj)
	}
	if a.boolean("warnings_only") {
		fields = append(fields, "type=Warning")
	}
	list, err := c.Clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: strings.Join(fields, ","),
		Limit:         defaultListLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list events in %s: %w", ns, err)
	}
	// Newest last so the most relevant events end the output.
	sort.Slice(list.Items, func(i, j int) bool {
		return eventTime(&list.Items[i]).Before(eventTime(&list.Items[j]))
	})
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		ev := &list.Items[i]
		out = append(out, map[string]any{
			"type":      ev.Type,
			"reason":    ev.Reason,
			"object":    fmt.Sprintf("%s/%s", ev.InvolvedObject.Kind, ev.InvolvedObject.Name),
			"message":   ev.Message,
			"count":     ev.Count,
			"last_seen": age(eventTime(ev)),
		})
	}
	return map[string]any{"namespace": ns, "count": len(out), "events": out}, nil
}

// --- Workloads ---

func handleListWorkloads(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	var out []map[string]any

	deps, err := c.Clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list deployments in %s: %w", ns, err)
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		entry := map[string]any{
			"kind": "Deployment", "name": d.Name, "namespace": ns,
			"ready":     fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, desired),
			"upToDate":  d.Status.UpdatedReplicas,
			"available": d.Status.AvailableReplicas,
			"age":       age(d.CreationTimestamp.Time),
		}
		if d.Status.ReadyReplicas < desired {
			entry["degraded_reason"] = deploymentProblem(d.Status.Conditions)
		}
		out = append(out, entry)
	}

	stss, err := c.Clientset.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list statefulsets in %s: %w", ns, err)
	}
	for i := range stss.Items {
		s := &stss.Items[i]
		desired := int32(1)
		if s.Spec.Replicas != nil {
			desired = *s.Spec.Replicas
		}
		out = append(out, map[string]any{
			"kind": "StatefulSet", "name": s.Name, "namespace": ns,
			"ready": fmt.Sprintf("%d/%d", s.Status.ReadyReplicas, desired),
			"age":   age(s.CreationTimestamp.Time),
		})
	}

	dss, err := c.Clientset.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list daemonsets in %s: %w", ns, err)
	}
	for i := range dss.Items {
		d := &dss.Items[i]
		out = append(out, map[string]any{
			"kind": "DaemonSet", "name": d.Name, "namespace": ns,
			"ready": fmt.Sprintf("%d/%d", d.Status.NumberReady, d.Status.DesiredNumberScheduled),
			"age":   age(d.CreationTimestamp.Time),
		})
	}
	return map[string]any{"namespace": ns, "count": len(out), "workloads": out}, nil
}

func handleGetWorkload(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns, kind, name := a.str("namespace"), strings.ToLower(a.str("kind")), a.str("name")
	var (
		selector   *metav1.LabelSelector
		status     map[string]any
		conditions []map[string]any
	)
	switch kind {
	case "deployment":
		d, err := c.Clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get deployment %s/%s: %w", ns, name, err)
		}
		selector = d.Spec.Selector
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		status = map[string]any{
			"desired": desired, "ready": d.Status.ReadyReplicas,
			"updated": d.Status.UpdatedReplicas, "available": d.Status.AvailableReplicas,
			"unavailable": d.Status.UnavailableReplicas,
		}
		for _, cond := range d.Status.Conditions {
			conditions = append(conditions, map[string]any{
				"type": string(cond.Type), "status": string(cond.Status),
				"reason": cond.Reason, "message": cond.Message,
			})
		}
	case "statefulset":
		s, err := c.Clientset.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get statefulset %s/%s: %w", ns, name, err)
		}
		selector = s.Spec.Selector
		desired := int32(1)
		if s.Spec.Replicas != nil {
			desired = *s.Spec.Replicas
		}
		status = map[string]any{
			"desired": desired, "ready": s.Status.ReadyReplicas,
			"updated": s.Status.UpdatedReplicas, "current": s.Status.CurrentReplicas,
		}
		for _, cond := range s.Status.Conditions {
			conditions = append(conditions, map[string]any{
				"type": string(cond.Type), "status": string(cond.Status),
				"reason": cond.Reason, "message": cond.Message,
			})
		}
	case "daemonset":
		d, err := c.Clientset.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get daemonset %s/%s: %w", ns, name, err)
		}
		selector = d.Spec.Selector
		status = map[string]any{
			"desired": d.Status.DesiredNumberScheduled, "ready": d.Status.NumberReady,
			"available": d.Status.NumberAvailable, "misscheduled": d.Status.NumberMisscheduled,
		}
		for _, cond := range d.Status.Conditions {
			conditions = append(conditions, map[string]any{
				"type": string(cond.Type), "status": string(cond.Status),
				"reason": cond.Reason, "message": cond.Message,
			})
		}
	default:
		return nil, fmt.Errorf("unknown kind %q (use deployment, statefulset, or daemonset)", kind)
	}

	// The workload's pods, via its selector.
	var pods []map[string]any
	if selector != nil {
		sel, err := metav1.LabelSelectorAsSelector(selector)
		if err == nil {
			list, lerr := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
			if lerr == nil {
				for i := range list.Items {
					pods = append(pods, podSummary(&list.Items[i]))
				}
			}
		}
	}
	return map[string]any{
		"kind": kind, "namespace": ns, "name": name,
		"status": status, "conditions": conditions, "pods": pods,
	}, nil
}

// --- Services & Routes ---

func handleListServices(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	list, err := c.Clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list services in %s: %w", ns, err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		svc := &list.Items[i]
		ports := make([]string, 0, len(svc.Spec.Ports))
		for _, p := range svc.Spec.Ports {
			ports = append(ports, fmt.Sprintf("%d->%s/%s", p.Port, p.TargetPort.String(), p.Protocol))
		}
		ready, notReady := endpointCounts(ctx, c, ns, svc.Name)
		out = append(out, map[string]any{
			"name": svc.Name, "namespace": ns, "type": string(svc.Spec.Type),
			"cluster_ip": svc.Spec.ClusterIP, "ports": ports,
			"endpoints_ready": ready, "endpoints_not_ready": notReady,
		})
	}
	return map[string]any{"namespace": ns, "count": len(out), "services": out}, nil
}

func handleGetService(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns, name := a.str("namespace"), a.str("name")
	svc, err := c.Clientset.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get service %s/%s: %w", ns, name, err)
	}
	slices, err := c.Clientset.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "kubernetes.io/service-name=" + name,
	})
	if err != nil {
		return nil, fmt.Errorf("list endpointslices for %s/%s: %w", ns, name, err)
	}
	var endpoints []map[string]any
	for i := range slices.Items {
		for _, ep := range slices.Items[i].Endpoints {
			ready := ep.Conditions.Ready != nil && *ep.Conditions.Ready
			entry := map[string]any{"addresses": ep.Addresses, "ready": ready}
			if ep.TargetRef != nil {
				entry["target"] = fmt.Sprintf("%s/%s", ep.TargetRef.Kind, ep.TargetRef.Name)
			}
			endpoints = append(endpoints, entry)
		}
	}
	return map[string]any{
		"namespace": ns, "name": name,
		"type": string(svc.Spec.Type), "cluster_ip": svc.Spec.ClusterIP,
		"selector": svc.Spec.Selector, "endpoints": endpoints,
	}, nil
}

var routeGVR = schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}

func handleListRoutes(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	list, err := c.Dynamic.Resource(routeGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list routes in %s (OpenShift Route API required): %w", ns, err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		r := &list.Items[i]
		host, _, _ := unstructured.NestedString(r.Object, "spec", "host")
		svc, _, _ := unstructured.NestedString(r.Object, "spec", "to", "name")
		tls, _, _ := unstructured.NestedString(r.Object, "spec", "tls", "termination")
		admitted := routeAdmitted(r)
		out = append(out, map[string]any{
			"name": r.GetName(), "namespace": ns, "host": host,
			"service": svc, "tls": tls, "admitted": admitted,
		})
	}
	return map[string]any{"namespace": ns, "count": len(out), "routes": out}, nil
}

// --- Storage & Quota ---

func handleListPVCs(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	list, err := c.Clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pvcs in %s: %w", ns, err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		pvc := &list.Items[i]
		capacity := ""
		if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			capacity = q.String()
		}
		class := ""
		if pvc.Spec.StorageClassName != nil {
			class = *pvc.Spec.StorageClassName
		}
		out = append(out, map[string]any{
			"name": pvc.Name, "namespace": ns, "phase": string(pvc.Status.Phase),
			"capacity": capacity, "storage_class": class, "volume": pvc.Spec.VolumeName,
			"age": age(pvc.CreationTimestamp.Time),
		})
	}
	return map[string]any{"namespace": ns, "count": len(out), "pvcs": out}, nil
}

func handleGetQuota(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	quotas, err := c.Clientset.CoreV1().ResourceQuotas(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list resourcequotas in %s: %w", ns, err)
	}
	qOut := make([]map[string]any, 0, len(quotas.Items))
	for i := range quotas.Items {
		q := &quotas.Items[i]
		usage := map[string]string{}
		for res, hard := range q.Status.Hard {
			used := q.Status.Used[res]
			usage[string(res)] = fmt.Sprintf("%s / %s", used.String(), hard.String())
		}
		qOut = append(qOut, map[string]any{"name": q.Name, "used_vs_hard": usage})
	}

	limits, err := c.Clientset.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list limitranges in %s: %w", ns, err)
	}
	lOut := make([]map[string]any, 0, len(limits.Items))
	for i := range limits.Items {
		lr := &limits.Items[i]
		items := make([]map[string]any, 0, len(lr.Spec.Limits))
		for _, item := range lr.Spec.Limits {
			items = append(items, map[string]any{
				"type":            string(item.Type),
				"default_limit":   quantities(item.Default),
				"default_request": quantities(item.DefaultRequest),
				"max":             quantities(item.Max),
			})
		}
		lOut = append(lOut, map[string]any{"name": lr.Name, "limits": items})
	}
	return map[string]any{"namespace": ns, "resource_quotas": qOut, "limit_ranges": lOut}, nil
}

// --- Metrics ---

var podMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}

func handleTopPods(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	list, err := c.Dynamic.Resource(podMetricsGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("pod metrics in %s (metrics-server/metrics.k8s.io required): %w", ns, err)
	}
	type usage struct {
		Pod      string `json:"pod"`
		CPUMilli int64  `json:"cpu_millicores"`
		MemoryMi int64  `json:"memory_mi"`
	}
	out := make([]usage, 0, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		containers, _, _ := unstructured.NestedSlice(item.Object, "containers")
		var cpu, mem int64
		for _, raw := range containers {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if s, found, _ := unstructured.NestedString(m, "usage", "cpu"); found {
				if q, qerr := resource.ParseQuantity(s); qerr == nil {
					cpu += q.MilliValue()
				}
			}
			if s, found, _ := unstructured.NestedString(m, "usage", "memory"); found {
				if q, qerr := resource.ParseQuantity(s); qerr == nil {
					mem += q.Value() / (1024 * 1024)
				}
			}
		}
		out = append(out, usage{Pod: item.GetName(), CPUMilli: cpu, MemoryMi: mem})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CPUMilli > out[j].CPUMilli })
	return map[string]any{"namespace": ns, "count": len(out), "pods": out}, nil
}

// --- Nodes ---

func handleListNodes(ctx context.Context, c *k8s.Client, _ args) (any, error) {
	list, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		node := &list.Items[i]
		ready := "Unknown"
		var pressures []string
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				ready = string(cond.Status)
			} else if cond.Status == corev1.ConditionTrue {
				pressures = append(pressures, string(cond.Type))
			}
		}
		out = append(out, map[string]any{
			"name": node.Name, "ready": ready, "roles": nodeRoles(node),
			"kubelet":  node.Status.NodeInfo.KubeletVersion,
			"cpu":      node.Status.Allocatable.Cpu().String(),
			"memory":   node.Status.Allocatable.Memory().String(),
			"taints":   len(node.Spec.Taints),
			"pressure": pressures,
			"age":      age(node.CreationTimestamp.Time),
		})
	}
	return map[string]any{"count": len(out), "nodes": out}, nil
}

func handleGetNode(ctx context.Context, c *k8s.Client, a args) (any, error) {
	name := a.str("name")
	node, err := c.Clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get node %s: %w", name, err)
	}
	conditions := make([]map[string]any, 0, len(node.Status.Conditions))
	for _, cond := range node.Status.Conditions {
		conditions = append(conditions, map[string]any{
			"type": string(cond.Type), "status": string(cond.Status),
			"reason": cond.Reason, "message": cond.Message,
		})
	}
	taints := make([]string, 0, len(node.Spec.Taints))
	for _, t := range node.Spec.Taints {
		taints = append(taints, fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect))
	}
	return map[string]any{
		"name": name, "roles": nodeRoles(node),
		"conditions": conditions, "taints": taints,
		"capacity":    quantities(node.Status.Capacity),
		"allocatable": quantities(node.Status.Allocatable),
		"node_info": map[string]string{
			"kubelet":           node.Status.NodeInfo.KubeletVersion,
			"os_image":          node.Status.NodeInfo.OSImage,
			"kernel":            node.Status.NodeInfo.KernelVersion,
			"container_runtime": node.Status.NodeInfo.ContainerRuntimeVersion,
		},
	}, nil
}

// --- Generic (dynamic) reads ---

func handleGetResource(ctx context.Context, c *k8s.Client, a args) (any, error) {
	gvr := schema.GroupVersionResource{
		Group: a.str("group"), Version: a.str("version"), Resource: a.str("resource"),
	}
	ns, name := a.str("namespace"), a.str("name")
	var obj *unstructured.Unstructured
	var err error
	if ns != "" {
		obj, err = c.Dynamic.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	} else {
		obj, err = c.Dynamic.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %q: %w", gvr, name, err)
	}
	// Strip the noisiest metadata before returning the object.
	unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
	unstructured.RemoveNestedField(obj.Object, "metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")
	return obj.Object, nil
}

func handleListResource(ctx context.Context, c *k8s.Client, a args) (any, error) {
	gvr := schema.GroupVersionResource{
		Group: a.str("group"), Version: a.str("version"), Resource: a.str("resource"),
	}
	opts := metav1.ListOptions{LabelSelector: a.str("selector"), Limit: 50}
	ns := a.str("namespace")
	var list *unstructured.UnstructuredList
	var err error
	if ns != "" {
		list, err = c.Dynamic.Resource(gvr).Namespace(ns).List(ctx, opts)
	} else {
		list, err = c.Dynamic.Resource(gvr).List(ctx, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", gvr, err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		out = append(out, map[string]any{
			"name":      item.GetName(),
			"namespace": item.GetNamespace(),
			"age":       age(item.GetCreationTimestamp().Time),
		})
	}
	return map[string]any{"resource": gvr.String(), "count": len(out), "items": out}, nil
}

// --- helpers ---

func podSummary(pod *corev1.Pod) map[string]any {
	readyCount, restarts := 0, int32(0)
	reason := ""
	for _, st := range pod.Status.ContainerStatuses {
		if st.Ready {
			readyCount++
		}
		restarts += st.RestartCount
		if st.State.Waiting != nil && st.State.Waiting.Reason != "" {
			reason = st.State.Waiting.Reason
		} else if st.State.Terminated != nil && st.State.Terminated.Reason != "" {
			reason = st.State.Terminated.Reason
		}
	}
	if pod.Status.Reason != "" {
		reason = pod.Status.Reason
	}
	entry := map[string]any{
		"name":      pod.Name,
		"namespace": pod.Namespace,
		"phase":     string(pod.Status.Phase),
		"ready":     fmt.Sprintf("%d/%d", readyCount, len(pod.Spec.Containers)),
		"restarts":  restarts,
		"node":      pod.Spec.NodeName,
		"age":       age(pod.CreationTimestamp.Time),
	}
	if reason != "" && reason != "Completed" {
		entry["reason"] = reason
	}
	return entry
}

func containerState(state corev1.ContainerState) string {
	switch {
	case state.Running != nil:
		return "Running since " + age(state.Running.StartedAt.Time) + " ago"
	case state.Waiting != nil:
		return fmt.Sprintf("Waiting (%s): %s", state.Waiting.Reason, state.Waiting.Message)
	case state.Terminated != nil:
		return fmt.Sprintf("Terminated (%s, exit %d)", state.Terminated.Reason, state.Terminated.ExitCode)
	}
	return ""
}

func objectEvents(ctx context.Context, c *k8s.Client, ns, kind, name string, limit int) ([]map[string]any, error) {
	list, err := c.Clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.kind=%s,involvedObject.name=%s", kind, name),
		Limit:         int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		ev := &list.Items[i]
		out = append(out, map[string]any{
			"type": ev.Type, "reason": ev.Reason, "message": ev.Message,
			"count": ev.Count, "last_seen": age(eventTime(ev)),
		})
	}
	return out, nil
}

func endpointCounts(ctx context.Context, c *k8s.Client, ns, svcName string) (ready, notReady int) {
	slices, err := c.Clientset.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "kubernetes.io/service-name=" + svcName,
	})
	if err != nil {
		return 0, 0
	}
	for i := range slices.Items {
		for _, ep := range slices.Items[i].Endpoints {
			if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
				ready++
			} else {
				notReady++
			}
		}
	}
	return ready, notReady
}

func routeAdmitted(r *unstructured.Unstructured) string {
	ingresses, _, _ := unstructured.NestedSlice(r.Object, "status", "ingress")
	for _, raw := range ingresses {
		ing, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		conds, _, _ := unstructured.NestedSlice(ing, "conditions")
		for _, craw := range conds {
			cond, ok := craw.(map[string]any)
			if !ok {
				continue
			}
			if cond["type"] == "Admitted" {
				return fmt.Sprintf("%v", cond["status"])
			}
		}
	}
	return "Unknown"
}

func deploymentProblem(conditions []appsv1.DeploymentCondition) string {
	for _, cond := range conditions {
		if (cond.Type == appsv1.DeploymentAvailable || cond.Type == appsv1.DeploymentProgressing) &&
			cond.Status != corev1.ConditionTrue {
			return fmt.Sprintf("%s: %s", cond.Reason, cond.Message)
		}
	}
	return ""
}

func quantities(rl corev1.ResourceList) map[string]string {
	if len(rl) == 0 {
		return nil
	}
	out := make(map[string]string, len(rl))
	for k, v := range rl {
		out[string(k)] = v.String()
	}
	return out
}

func eventTime(ev *corev1.Event) time.Time {
	if !ev.LastTimestamp.IsZero() {
		return ev.LastTimestamp.Time
	}
	if !ev.EventTime.IsZero() {
		return ev.EventTime.Time
	}
	return ev.CreationTimestamp.Time
}

func nodeRoles(node *corev1.Node) []string {
	var roles []string
	for label := range node.Labels {
		if role, ok := strings.CutPrefix(label, "node-role.kubernetes.io/"); ok && role != "" {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	return roles
}

// age renders a compact duration since t (e.g. "3d4h", "12m").
func age(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
