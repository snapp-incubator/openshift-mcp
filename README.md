# openshift-mcp

Read-only OpenShift/Kubernetes observability MCP server. Gives an AI agent
(the [SnappCloud bot](../snappcloud-bot)) cluster vision: pods, workloads,
services, OpenShift routes, events, container logs, quotas, PVCs, nodes, and
live pod metrics — summarized for LLM consumption. Built on the official
[MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
(streamable-HTTP, stateless).

**Strictly read-only.** The ClusterRole grants only `get`/`list`; the server
diagnoses issues and lets the agent recommend fixes — it never mutates
cluster state.

## Tools (16)

| Tool | Purpose |
|------|---------|
| `list_pods` | Pods with phase/ready/restarts/node + failure reason |
| `get_pod` | One pod in depth: containers, states, conditions, resources, events |
| `pod_logs` | Container logs; `previous=true` for pre-crash logs |
| `list_events` | Namespace events (scheduling, image pull, OOM, probes) |
| `list_workloads` | Deployments/StatefulSets/DaemonSets ready-vs-desired |
| `get_workload` | Rollout state, conditions, and the workload's pods |
| `list_services` / `get_service` | Services + endpoint readiness (selector mismatches) |
| `list_routes` | OpenShift Routes: host, target, TLS, admitted |
| `list_certificates` | Cert expiry from TLS secrets: subject/issuer/SANs/notAfter/days_left (public cert only, never keys). Needs secret-read Role in the namespace |
| `list_pvcs` | PVC phase/capacity/class |
| `get_quota` | ResourceQuota usage + LimitRanges |
| `top_pods` | Live CPU/memory per pod (metrics.k8s.io) |
| `list_nodes` / `get_node` | Node readiness, pressure, taints, capacity |
| `get_resource` / `list_resource` | Generic read for any CRD/OpenShift kind (within RBAC) |

Most tools take a required `namespace` — which lets the bot's authorization
layer enforce the caller's scope on both arguments and results.

## Run

```bash
make run-http                 # streamable HTTP on :8080/mcp
make run-mcp                  # stdio (local MCP clients)
K8S_KUBECONFIG=~/.kube/config make run-http   # outside a cluster
```

Env: `K8S_KUBECONFIG`, `K8S_CONTEXT`, `K8S_TIMEOUT` (30s), `K8S_QPS` (50),
`K8S_BURST` (100). In-cluster it uses the ServiceAccount.

## Deploy

The Helm chart lives in the ArgoCD apps repo at `core/helm/apps/openshift-mcp`
(Deployment, Service, ServiceAccount, read-only ClusterRole/Binding, private
HTTPProxy at `openshift-mcp.apps.private.<region_hostname>/mcp`, NetworkPolicy
restricted to the ingress namespace) and is registered in
`newcluster-bootstrap`. Add each deployed region to the bot's
`agent.clusters[].servers` as `- name: k8s`. Extra CRDs can be exposed through
`get_resource`/`list_resource` via read-only rules in `rbac.extraRules`.
