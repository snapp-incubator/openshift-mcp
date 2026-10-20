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

## Tools (19)

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
| `disruption_audit` | Cluster-wide drain-safety inventory: every PDB with status, owners and overlaps; every RabbitMQ/Redis/quorum-system instance with replicas and covering PDBs |
| `list_teams` | Team CRs: admins and namespaces with their environment |

Most tools take a required `namespace` — which lets the bot's authorization
layer enforce the caller's scope on both arguments and results.

`disruption_audit` and `list_teams` are the exception: cluster-wide by design,
with no namespace to enforce on. The bot marks them `clusterAdminOnly`.

### Disruption audit

`disruption_audit` is the data source for the bot's weekly best-practice report
(single-instance brokers/caches, drain-blocking and below-quorum PDBs). It
reports facts; the rules that turn them into findings live in the bot. The
server does the joins so the caller never pulls every pod:

- **PDBs**: spec (`minAvailable`/`maxUnavailable` exactly as written,
  `unhealthyPodEvictionPolicy`) and the disruption controller's computed status
  (`expectedPods`, `currentHealthy`, `desiredHealthy`, `disruptionsAllowed`), so
  percentages and rounding are already resolved the way eviction resolves them.
  `owners` are the controllers of the selected pods, with ReplicaSets resolved
  to their Deployment and with `replicas`, `images` and `operatorKind`.
  `overlapsWith` lists the other PDBs selecting the same running pod: eviction
  refuses such a pod outright. Selectors follow eviction semantics (nil matches
  nothing, `{}` matches the whole namespace), and pending, terminating and
  finished pods never count as overlap, because eviction does not check them.
- **Instances**: `RabbitmqCluster` (`spec.replicas`, default 1) and the opstree
  `Redis`, `RedisReplication`, `RedisCluster` (leaders) and `RedisSentinel` CRs
  (`source: operator`), plus Deployments and StatefulSets with no controlling
  owner whose image names `redis`, `rabbitmq` or a quorum image
  (`source: image`). `quorum_images` overrides the quorum keyword list; only
  StatefulSets count as quorum systems. `pdbs` is matched against the pod
  template, so a member that is down at scan time does not change it.
- **namespaces**: `snappcloud.io/team` and `environment` for every namespace
  mentioned; an unlabelled one comes back as `{}`.
- **notInstalled**: CRDs this cluster does not serve. Any other list error,
  including a forbidden one, fails the call: a partial inventory would read as
  "nothing wrong".

Every list is paged with `continue` tokens, so nothing is truncated. The
`snappcloud.io/best-practice-ignore` annotation on a PDB, CR or workload is
passed through as `ignore`.

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
