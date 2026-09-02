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

### Redaction

`get_resource` is the only tool that returns a raw object, so it is the only
path an inline credential can take into an answer. Before returning, it replaces
credential **values** — leaving the fields that hold them in place:

- fields named for a credential (`password`, `token`, `clientSecret`, `apiKey`,
  `authorization`, …), including `env` entries whose *name* is credential-like;
- URL userinfo: `https://user:pass@host` → `https://user:[redacted]@host`, so the
  endpoint stays visible;
- sensitive command-line flags (`-remoteWrite.basicAuth.password=…`), the form
  operator `extraArgs` use;
- an inline Route TLS private key (`spec.tls.key`), recognised by its siblings —
  the certificate is public and is kept.

References are never redacted: `secretKeyRef`, `secretName` and friends name a
Secret rather than carrying one, and "which secret does this use" has to stay
answerable. Detection is name-driven, never entropy-driven — guessing at
"random-looking" strings would redact image digests, UIDs and resource versions.

The response reports how many values were replaced, so the model can say a
password is set inline without revealing it.

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
