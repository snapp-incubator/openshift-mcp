package mcp

// Schema helpers: JSON-Schema fragments for tool inputs.

func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func strEnum(desc string, values ...string) map[string]any {
	vals := make([]any, len(values))
	for i, v := range values {
		vals[i] = v
	}
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}

func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

const nsDesc = "Kubernetes namespace."

func buildTools() []tool {
	return []tool{
		{
			name:        "list_pods",
			description: "List pods in a namespace with phase, readiness, restarts, node, and a reason when not healthy. Start here for any workload issue.",
			schema: objSchema(map[string]any{
				"namespace":      str(nsDesc),
				"selector":       str("Optional label selector (e.g. 'app=web,tier=frontend')."),
				"field_selector": str("Optional field selector (e.g. 'status.phase=Pending' or 'spec.nodeName=node1')."),
				"limit":          integer("Max pods to return (default 100)."),
			}, "namespace"),
			handler: handleListPods,
		},
		{
			name:        "get_pod",
			description: "Full diagnostic view of one pod: containers (image, state, restart reason, resources), conditions, QoS, and its recent events.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
				"name":      str("Pod name."),
			}, "namespace", "name"),
			handler: handleGetPod,
		},
		{
			name:        "pod_logs",
			description: "Fetch container logs from a pod. Use previous=true to read logs from before the last restart (essential for CrashLoopBackOff).",
			schema: objSchema(map[string]any{
				"namespace":  str(nsDesc),
				"name":       str("Pod name."),
				"container":  str("Container name. Defaults to the first container."),
				"tail_lines": integer("Number of trailing lines (default 100, max 2000)."),
				"previous":   boolean("Read logs of the previous (crashed) container instance."),
			}, "namespace", "name"),
			handler: handlePodLogs,
		},
		{
			name:        "list_events",
			description: "Recent events in a namespace: scheduling failures, image pull errors, OOM kills, probe failures. Filter to one object or warnings only.",
			schema: objSchema(map[string]any{
				"namespace":     str(nsDesc),
				"object":        str("Optional involved object name (e.g. a pod or deployment name)."),
				"warnings_only": boolean("Return only Warning events."),
			}, "namespace"),
			handler: handleListEvents,
		},
		{
			name:        "list_workloads",
			description: "Deployments, StatefulSets, and DaemonSets in a namespace with ready/desired replica counts. Quickly shows which workload is degraded.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleListWorkloads,
		},
		{
			name:        "get_workload",
			description: "One workload's rollout state: replica counts, conditions (with messages), selector, and its pods' health.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
				"kind":      strEnum("Workload kind.", "deployment", "statefulset", "daemonset"),
				"name":      str("Workload name."),
			}, "namespace", "kind", "name"),
			handler: handleGetWorkload,
		},
		{
			name:        "list_services",
			description: "Services in a namespace with type, cluster IP, ports, and ready endpoint counts (0 ready endpoints = broken service).",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleListServices,
		},
		{
			name:        "get_service",
			description: "One service's spec plus its endpoints: which backend addresses are ready/not-ready. Diagnose selector mismatches and unready backends.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
				"name":      str("Service name."),
			}, "namespace", "name"),
			handler: handleGetService,
		},
		{
			name:        "list_routes",
			description: "OpenShift Routes in a namespace: host, target service/port, TLS termination, and admitted status.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleListRoutes,
		},
		{
			name:        "list_pvcs",
			description: "PersistentVolumeClaims in a namespace: phase (Pending = unbound), capacity, storage class, bound volume.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleListPVCs,
		},
		{
			name:        "get_quota",
			description: "ResourceQuota usage (hard vs used) and LimitRanges in a namespace. Exhausted quota silently blocks new pods and rollouts.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleGetQuota,
		},
		{
			name:        "top_pods",
			description: "Live CPU (millicores) and memory (Mi) usage per pod in a namespace via metrics.k8s.io, sorted by CPU. Compare against requests/limits from get_pod.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleTopPods,
		},
		{
			name:        "list_nodes",
			description: "Cluster nodes: readiness, roles, kubelet version, allocatable CPU/memory, taints, and any pressure conditions.",
			schema:      objSchema(map[string]any{}),
			handler:     handleListNodes,
		},
		{
			name:        "get_node",
			description: "One node in depth: all conditions (memory/disk/PID pressure), taints, capacity vs allocatable, addresses, system info.",
			schema: objSchema(map[string]any{
				"name": str("Node name."),
			}, "name"),
			handler: handleGetNode,
		},
		{
			name:        "list_certificates",
			description: "Certificate expiry report for a namespace: parses the PUBLIC certificate data (tls.crt/ca.crt) of TLS secrets and returns subject, issuer, SANs, notBefore/notAfter, and days until expiry. Never returns private keys. Requires read access to secrets in that namespace (granted only for platform namespaces, e.g. openshift-ingress, openshift-config).",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleListCertificates,
		},
		{
			name:        "get_resource",
			description: "Read any single API object (including CRDs and OpenShift kinds) by group/version/resource plural. Read-only escape hatch, e.g. group='route.openshift.io', version='v1', resource='routes'.",
			schema: objSchema(map[string]any{
				"group":     str("API group ('' for core, e.g. 'apps', 'route.openshift.io')."),
				"version":   str("API version (e.g. 'v1')."),
				"resource":  str("Resource plural (e.g. 'pods', 'routes', 'ciliumnetworkpolicies')."),
				"namespace": str("Namespace. Omit for cluster-scoped resources."),
				"name":      str("Object name."),
			}, "version", "resource", "name"),
			handler: handleGetResource,
		},
		{
			name:        "list_resource",
			description: "List any resource type (including CRDs and OpenShift kinds) by group/version/resource plural. Returns name/namespace/age summaries, capped at 50 items.",
			schema: objSchema(map[string]any{
				"group":     str("API group ('' for core)."),
				"version":   str("API version (e.g. 'v1')."),
				"resource":  str("Resource plural."),
				"namespace": str("Namespace. Omit for cluster-scoped resources."),
				"selector":  str("Optional label selector."),
			}, "version", "resource"),
			handler: handleListResource,
		},
	}
}
