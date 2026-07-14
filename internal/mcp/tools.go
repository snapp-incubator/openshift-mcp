package mcp


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

func strArray(desc string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": desc,
		"items":       map[string]any{"type": "string"},
	}
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
			description: "Deployments, StatefulSets, DaemonSets, Jobs, and CronJobs in a namespace with ready/desired counts and a reason when degraded. Quickly shows which workload is broken.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
				"kinds": strArray("Optional kinds to include: deployment, statefulset, daemonset, job, cronjob, " +
					"deploymentconfig (OpenShift). Defaults to all of them."),
			}, "namespace"),
			handler: handleListWorkloads,
		},
		{
			name:        "get_workload",
			description: "One workload's rollout state: replica or completion counts, conditions (with messages), selector, and its pods' health.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
				"kind": strEnum("Workload kind.",
					"deployment", "statefulset", "daemonset", "job", "cronjob", "replicaset"),
				"name": str("Workload name."),
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
			description: "PersistentVolumeClaims in a namespace: phase (Pending = unbound), capacity, storage class, bound volume. When any PVC is unbound it also returns the cluster's StorageClasses and flags a missing or ambiguous default class.",
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
			description: "Cluster nodes: readiness, roles, kubelet version, allocatable CPU/memory, taints, and any pressure conditions. Set include_usage=true to add live CPU/memory per node as a percentage of allocatable (which nodes are saturated).",
			schema: objSchema(map[string]any{
				"include_usage": boolean("Include live CPU/memory usage per node from metrics.k8s.io, as a percentage of allocatable."),
			}),
			handler: handleListNodes,
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
			name:        "diagnose_namespace",
			description: "One-shot health sweep of a namespace: checks pods, workloads, service endpoints, quota, and PVCs, and returns only the problems, ranked worst-first, each with the tool to call next. Start here when asked an open-ended 'what is wrong with X'.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleDiagnoseNamespace,
		},
		{
			name:        "list_config",
			description: "ConfigMaps and Secrets in a namespace with their key names — the thing to check for CreateContainerConfigError, where a pod references a missing object or key. Secret values are never exposed; ConfigMap values are available via get_resource.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
				"selector":  str("Optional label selector."),
			}, "namespace"),
			handler: handleListConfig,
		},
		{
			name:        "list_network_policies",
			description: "NetworkPolicies in a namespace, with each policy's target pods and its ingress/egress rules in readable form. The first thing to check when one pod cannot reach another.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleListNetworkPolicies,
		},
		{
			name:        "list_ingresses",
			description: "Kubernetes Ingresses in a namespace: host/path rules, backend services, TLS hosts, and assigned addresses. No address means no controller claimed it. On OpenShift, list_routes is usually the relevant tool instead.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleListIngresses,
		},
		{
			name:        "list_rbac",
			description: "ServiceAccounts, Roles, and RoleBindings in a namespace — who is granted what. Use when a pod gets 403/Forbidden from the Kubernetes API.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleListRBAC,
		},
		{
			name:        "list_machines",
			description: "OpenShift Machines (the layer beneath nodes) with phase and their backing node. Explains a node that never joined or disappeared, which list_nodes cannot show.",
			schema: objSchema(map[string]any{
				"unhealthy_only": boolean("Return only machines that are not Running."),
			}),
			handler: handleListMachines,
		},
		{
			name:        "list_hpas",
			description: "HorizontalPodAutoscalers in a namespace: min/max, current vs desired replicas, metric targets vs current values, and conditions explaining why scaling is blocked or limited.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleListHPAs,
		},
		{
			name:        "list_pdbs",
			description: "PodDisruptionBudgets in a namespace with allowed disruptions and healthy/desired counts. disruptions_allowed=0 is why a node drain or upgrade hangs.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleListPDBs,
		},
		{
			name:        "list_namespaces",
			description: "Namespaces visible to this server, with phase and age. Use to discover what exists before drilling in.",
			schema: objSchema(map[string]any{
				"filter":   str("Optional case-insensitive substring match on the namespace name."),
				"selector": str("Optional label selector."),
			}),
			handler: handleListNamespaces,
		},
		{
			name:        "api_resources",
			description: "Discover which API resources this cluster serves, with the exact group/version/resource plural that get_resource and list_resource need. Call this before guessing those values for a CRD or OpenShift kind.",
			schema: objSchema(map[string]any{
				"filter": str("Optional case-insensitive substring match on resource name, kind, or group (e.g. 'route', 'cilium')."),
				"group":  str("Optional exact API group filter (e.g. 'route.openshift.io')."),
			}),
			handler: handleAPIResources,
		},
		{
			name:        "list_cluster_operators",
			description: "OpenShift ClusterOperators with Available/Degraded/Progressing and the message from any failing condition. When many namespaces break at once, check here before blaming a workload.",
			schema: objSchema(map[string]any{
				"unhealthy_only": boolean("Return only operators that are Degraded or not Available."),
			}),
			handler: handleListClusterOperators,
		},
		{
			name:        "get_cluster_version",
			description: "OpenShift cluster version, channel, upgrade progress, available updates, and recent upgrade history.",
			schema:      objSchema(map[string]any{}),
			handler:     handleGetClusterVersion,
		},
		{
			name:        "list_builds",
			description: "OpenShift Builds in a namespace with phase, reason, and failure message. A failed build explains why the image a workload wants was never produced.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
				"selector":  str("Optional label selector (e.g. 'buildconfig=my-app')."),
			}, "namespace"),
			handler: handleListBuilds,
		},
		{
			name:        "list_imagestreams",
			description: "OpenShift ImageStreams in a namespace with their tags, flagging tags that resolve to no image — a common cause of pods that never pull.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleListImageStreams,
		},
		{
			name:        "get_resource",
			description: "Read any single API object (including CRDs and OpenShift kinds) by group/version/resource plural. Read-only escape hatch, e.g. group='route.openshift.io', version='v1', resource='routes'. Use api_resources to find the exact values.",
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
