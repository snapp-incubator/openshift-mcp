package mcp

// Disruption audit: the data behind the bot's weekly best-practice report.
// Pods are matched to PDBs and owners here, so the caller never has to fetch
// every pod. The tools only report facts; the caller decides what is a problem.

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

const (
	// Namespace labels set by team-operator.
	labelTeam = "snappcloud.io/team"
	labelEnv  = "environment"
	// Teams set this to silence rules on an object; passed through as-is.
	annotationIgnore = "snappcloud.io/best-practice-ignore"
)

// defaultQuorumImages are image keywords of systems that stop working when
// most of their members are down at once.
var defaultQuorumImages = []string{
	"rabbitmq", "etcd", "zookeeper", "kafka", "consul", "nats", "mongo", "redis-sentinel", "elasticsearch",
}

// cacheEngines are the caches and brokers detected by image on workloads
// that no operator manages.
var cacheEngines = []string{"redis", "rabbitmq"}

// dataServiceKind describes an operator CR the audit reports on.
type dataServiceKind struct {
	group, resource, kind, engine string
	versions                      []string
	quorum                        bool
	// replicas returns the desired member count.
	replicas func(*unstructured.Unstructured) int64
	// member picks which owned StatefulSet holds the members; nil means any.
	member func(cr, sts string) bool
}

const opstreeGroup = "redis.redis.opstreelabs.in"

// Older opstree operators serve only v1beta1.
var opstreeVersions = []string{"v1beta2", "v1beta1"}

var dataServiceKinds = []dataServiceKind{
	{
		group: "rabbitmq.com", versions: []string{"v1beta1"}, resource: "rabbitmqclusters",
		kind: "RabbitmqCluster", engine: "rabbitmq", quorum: true,
		// Defaults to 1 when unset.
		replicas: func(o *unstructured.Unstructured) int64 { return nestedInt(o.Object, 1, "spec", "replicas") },
	},
	{
		group: opstreeGroup, versions: opstreeVersions, resource: "redis",
		kind: "Redis", engine: "redis",
		// Standalone Redis always runs one instance.
		replicas: func(*unstructured.Unstructured) int64 { return 1 },
	},
	{
		group: opstreeGroup, versions: opstreeVersions, resource: "redisreplications",
		kind: "RedisReplication", engine: "redis",
		replicas: func(o *unstructured.Unstructured) int64 { return nestedInt(o.Object, 0, "spec", "clusterSize") },
	},
	{
		group: opstreeGroup, versions: opstreeVersions, resource: "redisclusters",
		kind: "RedisCluster", engine: "redis", quorum: true,
		// Only leaders count; redisLeader.replicas overrides clusterSize.
		replicas: func(o *unstructured.Unstructured) int64 {
			return nestedInt(o.Object, nestedInt(o.Object, 0, "spec", "clusterSize"), "spec", "redisLeader", "replicas")
		},
		member: func(cr, sts string) bool { return sts == cr+"-leader" },
	},
	{
		group: opstreeGroup, versions: opstreeVersions, resource: "redissentinels",
		kind: "RedisSentinel", engine: "redis-sentinel", quorum: true,
		replicas: func(o *unstructured.Unstructured) int64 { return nestedInt(o.Object, 0, "spec", "clusterSize") },
	},
}

type auditOwner struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Set only for owners that are not Deployments or StatefulSets.
	APIVersion   string   `json:"apiVersion,omitempty"`
	Replicas     *int32   `json:"replicas,omitempty"`
	Images       []string `json:"images,omitempty"`
	OperatorKind string   `json:"operatorKind,omitempty"`
	Ignore       string   `json:"ignore,omitempty"`
}

type auditPDB struct {
	Namespace                  string              `json:"ns"`
	Name                       string              `json:"name"`
	MinAvailable               *intstr.IntOrString `json:"minAvailable,omitempty"`
	MaxUnavailable             *intstr.IntOrString `json:"maxUnavailable,omitempty"`
	UnhealthyPodEvictionPolicy string              `json:"unhealthyPodEvictionPolicy,omitempty"`
	ExpectedPods               int32               `json:"expectedPods"`
	CurrentHealthy             int32               `json:"currentHealthy"`
	DesiredHealthy             int32               `json:"desiredHealthy"`
	DisruptionsAllowed         int32               `json:"disruptionsAllowed"`
	Owners                     []auditOwner        `json:"owners"`
	OverlapsWith               []string            `json:"overlapsWith,omitempty"`
	Ignore                     string              `json:"ignore,omitempty"`

	selector labels.Selector
	owners   map[ownerRef]bool
	overlaps map[string]bool
}

type auditInstance struct {
	Namespace string `json:"ns"`
	Name      string `json:"name"`
	Engine    string `json:"engine"`
	Kind      string `json:"kind"`
	// The StatefulSet running an operator instance's members.
	Workload string   `json:"workload,omitempty"`
	Replicas int64    `json:"replicas"`
	Quorum   bool     `json:"quorum"`
	Source   string   `json:"source"`
	PDBs     []string `json:"pdbs"`
	Images   []string `json:"images,omitempty"`
	Ignore   string   `json:"ignore,omitempty"`
}

type nsOwner struct {
	Team string `json:"team,omitempty"`
	Env  string `json:"env,omitempty"`
}

// ownerRef is the controller of a pod.
type ownerRef struct{ ns, kind, name, apiVersion string }

// workload holds the Deployment or StatefulSet fields the audit uses.
type workload struct {
	kind, ns, name string
	replicas       int32
	images         []string
	template       labels.Set
	controller     *metav1.OwnerReference
	ignore         string
}

func (w *workload) owner() auditOwner {
	r := w.replicas
	o := auditOwner{Kind: w.kind, Name: w.name, Replicas: &r, Images: w.images, Ignore: w.ignore}
	if w.controller != nil {
		o.OperatorKind = w.controller.Kind
	}
	return o
}

func handleDisruptionAudit(ctx context.Context, c *k8s.Client, a args) (any, error) {
	quorumImages := defaultQuorumImages
	if custom := a.strs("quorum_images"); len(custom) > 0 {
		quorumImages = make([]string, len(custom))
		for i, img := range custom {
			quorumImages[i] = strings.ToLower(img)
		}
	}

	// Step 1: independent lists, in parallel.
	var (
		pdbs         []*auditPDB
		workloads    []*workload
		nsLabels     map[string]map[string]string
		crs          = make([][]*unstructured.Unstructured, len(dataServiceKinds))
		crsInstalled = make([]bool, len(dataServiceKinds))
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { pdbs, err = listPDBs(gctx, c); return err })
	g.Go(func() (err error) { workloads, err = listAuditWorkloads(gctx, c); return err })
	g.Go(func() (err error) { nsLabels, err = listNamespaceLabels(gctx, c); return err })
	for i, kind := range dataServiceKinds {
		g.Go(func() (err error) {
			crsInstalled[i], err = c.ListCustom(gctx, kind.group, kind.resource, kind.versions,
				func(o *unstructured.Unstructured) { crs[i] = append(crs[i], o) })
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	byNS := map[string][]*auditPDB{}
	for _, p := range pdbs {
		byNS[p.Namespace] = append(byNS[p.Namespace], p)
	}

	// Step 2: pods and ReplicaSets, kept only in namespaces that have a PDB.
	var rsDeployment map[string]string
	g, gctx = errgroup.WithContext(ctx)
	g.Go(func() (err error) { rsDeployment, err = listReplicaSetDeployments(gctx, c, byNS); return err })
	g.Go(func() error { return scanPods(gctx, c, byNS) })
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Step 3: join everything together.
	wlIndex := make(map[string]*workload, len(workloads))
	ownedSTS := map[types.UID][]*workload{}
	for _, w := range workloads {
		wlIndex[w.kind+"/"+w.ns+"/"+w.name] = w
		if w.kind == "StatefulSet" && w.controller != nil {
			ownedSTS[w.controller.UID] = append(ownedSTS[w.controller.UID], w)
		}
	}
	for _, p := range pdbs {
		p.Owners = resolveOwners(p.owners, rsDeployment, wlIndex)
		for name := range p.overlaps {
			p.OverlapsWith = append(p.OverlapsWith, name)
		}
		sort.Strings(p.OverlapsWith)
	}

	instances := []auditInstance{}
	notInstalled := []string{}
	for i, kind := range dataServiceKinds {
		if !crsInstalled[i] {
			notInstalled = append(notInstalled, kind.resource+"."+kind.group)
			continue
		}
		for _, cr := range crs[i] {
			inst := auditInstance{
				Namespace: cr.GetNamespace(), Name: cr.GetName(),
				Engine: kind.engine, Kind: kind.kind, Replicas: kind.replicas(cr),
				Quorum: kind.quorum, Source: "operator", PDBs: []string{},
				Ignore: cr.GetAnnotations()[annotationIgnore],
			}
			for _, sts := range ownedSTS[cr.GetUID()] {
				if kind.member == nil || kind.member(cr.GetName(), sts.name) {
					inst.Workload = sts.name
					inst.PDBs = coveringPDBs(byNS[sts.ns], sts.template)
					break
				}
			}
			instances = append(instances, inst)
		}
	}
	instances = append(instances, imageInstances(workloads, byNS, quorumImages)...)
	sort.Slice(instances, func(i, j int) bool {
		a, b := instances[i], instances[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Kind < b.Kind
	})

	// Every namespace mentioned gets an entry, empty if it has no team.
	namespaces := map[string]nsOwner{}
	for _, p := range pdbs {
		namespaces[p.Namespace] = nsOwner{Team: nsLabels[p.Namespace][labelTeam], Env: nsLabels[p.Namespace][labelEnv]}
	}
	for _, inst := range instances {
		namespaces[inst.Namespace] = nsOwner{Team: nsLabels[inst.Namespace][labelTeam], Env: nsLabels[inst.Namespace][labelEnv]}
	}

	return map[string]any{
		"pdbs":         pdbs,
		"instances":    instances,
		"namespaces":   namespaces,
		"notInstalled": notInstalled,
		"quorumImages": quorumImages,
	}, nil
}

func listPDBs(ctx context.Context, c *k8s.Client) ([]*auditPDB, error) {
	out := []*auditPDB{}
	err := k8s.ListPages(func(opts metav1.ListOptions) (string, error) {
		list, err := c.Clientset.PolicyV1().PodDisruptionBudgets("").List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list poddisruptionbudgets: %w", err)
		}
		for i := range list.Items {
			out = append(out, newAuditPDB(&list.Items[i]))
		}
		return list.Continue, nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, err
}

func newAuditPDB(pdb *policyv1.PodDisruptionBudget) *auditPDB {
	// Match like eviction does: a missing or invalid selector matches nothing,
	// an empty one matches every pod in the namespace.
	sel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		sel = labels.Nothing()
	}
	p := &auditPDB{
		Namespace:          pdb.Namespace,
		Name:               pdb.Name,
		MinAvailable:       pdb.Spec.MinAvailable,
		MaxUnavailable:     pdb.Spec.MaxUnavailable,
		ExpectedPods:       pdb.Status.ExpectedPods,
		CurrentHealthy:     pdb.Status.CurrentHealthy,
		DesiredHealthy:     pdb.Status.DesiredHealthy,
		DisruptionsAllowed: pdb.Status.DisruptionsAllowed,
		Owners:             []auditOwner{},
		Ignore:             pdb.Annotations[annotationIgnore],
		selector:           sel,
		owners:             map[ownerRef]bool{},
		overlaps:           map[string]bool{},
	}
	if pdb.Spec.UnhealthyPodEvictionPolicy != nil {
		p.UnhealthyPodEvictionPolicy = string(*pdb.Spec.UnhealthyPodEvictionPolicy)
	}
	return p
}

func listAuditWorkloads(ctx context.Context, c *k8s.Client) ([]*workload, error) {
	var out []*workload
	err := k8s.ListPages(func(opts metav1.ListOptions) (string, error) {
		list, err := c.Clientset.AppsV1().Deployments("").List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list deployments: %w", err)
		}
		for i := range list.Items {
			d := &list.Items[i]
			out = append(out, newWorkload("Deployment", &d.ObjectMeta, d.Spec.Replicas, &d.Spec.Template))
		}
		return list.Continue, nil
	})
	if err != nil {
		return nil, err
	}
	err = k8s.ListPages(func(opts metav1.ListOptions) (string, error) {
		list, err := c.Clientset.AppsV1().StatefulSets("").List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list statefulsets: %w", err)
		}
		for i := range list.Items {
			s := &list.Items[i]
			out = append(out, newWorkload("StatefulSet", &s.ObjectMeta, s.Spec.Replicas, &s.Spec.Template))
		}
		return list.Continue, nil
	})
	return out, err
}

func newWorkload(kind string, meta *metav1.ObjectMeta, replicas *int32, tmpl *corev1.PodTemplateSpec) *workload {
	w := &workload{
		kind: kind, ns: meta.Namespace, name: meta.Name, replicas: 1,
		template:   labels.Set(tmpl.Labels),
		controller: metav1.GetControllerOfNoCopy(meta),
		ignore:     meta.Annotations[annotationIgnore],
	}
	if w.controller != nil {
		ref := *w.controller
		w.controller = &ref
	}
	if replicas != nil {
		w.replicas = *replicas
	}
	for _, ct := range tmpl.Spec.Containers {
		if !slices.Contains(w.images, ct.Image) {
			w.images = append(w.images, ct.Image)
		}
	}
	return w
}

func listNamespaceLabels(ctx context.Context, c *k8s.Client) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	err := k8s.ListPages(func(opts metav1.ListOptions) (string, error) {
		list, err := c.Clientset.CoreV1().Namespaces().List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list namespaces: %w", err)
		}
		for i := range list.Items {
			out[list.Items[i].Name] = list.Items[i].Labels
		}
		return list.Continue, nil
	})
	return out, err
}

// listReplicaSetDeployments maps "ns/replicaset" to its Deployment's name.
func listReplicaSetDeployments(ctx context.Context, c *k8s.Client, byNS map[string][]*auditPDB) (map[string]string, error) {
	out := map[string]string{}
	err := k8s.ListPages(func(opts metav1.ListOptions) (string, error) {
		list, err := c.Clientset.AppsV1().ReplicaSets("").List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list replicasets: %w", err)
		}
		for i := range list.Items {
			rs := &list.Items[i]
			if len(byNS[rs.Namespace]) == 0 {
				continue
			}
			if ref := metav1.GetControllerOfNoCopy(rs); ref != nil && ref.Kind == "Deployment" {
				out[rs.Namespace+"/"+rs.Name] = ref.Name
			}
		}
		return list.Continue, nil
	})
	return out, err
}

// scanPods records on each PDB the owners of its pods and any other PDB that
// selects the same pods.
func scanPods(ctx context.Context, c *k8s.Client, byNS map[string][]*auditPDB) error {
	return k8s.ListPages(func(opts metav1.ListOptions) (string, error) {
		list, err := c.Clientset.CoreV1().Pods("").List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list pods: %w", err)
		}
		for i := range list.Items {
			pod := &list.Items[i]
			nsPDBs := byNS[pod.Namespace]
			if len(nsPDBs) == 0 || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			set := labels.Set(pod.Labels)
			var matched []*auditPDB
			for _, p := range nsPDBs {
				if p.selector.Matches(set) {
					matched = append(matched, p)
				}
			}
			if len(matched) == 0 {
				continue
			}
			ref := ownerRef{ns: pod.Namespace, kind: "Pod", name: pod.Name}
			if ctl := metav1.GetControllerOfNoCopy(pod); ctl != nil {
				ref = ownerRef{ns: pod.Namespace, kind: ctl.Kind, name: ctl.Name, apiVersion: ctl.APIVersion}
			}
			for _, p := range matched {
				p.owners[ref] = true
			}
			// Eviction ignores PDBs for pending and deleting pods, so they
			// cannot be blocked by an overlap.
			if len(matched) > 1 && pod.Status.Phase != corev1.PodPending && pod.DeletionTimestamp == nil {
				for _, p := range matched {
					for _, q := range matched {
						if p != q {
							p.overlaps[q.Name] = true
						}
					}
				}
			}
		}
		return list.Continue, nil
	})
}

func resolveOwners(refs map[ownerRef]bool, rsDeployment map[string]string, wlIndex map[string]*workload) []auditOwner {
	seen := map[string]bool{}
	out := []auditOwner{}
	for ref := range refs {
		kind, name := ref.kind, ref.name
		if kind == "ReplicaSet" {
			if d, ok := rsDeployment[ref.ns+"/"+name]; ok {
				kind, name = "Deployment", d
			}
		}
		key := kind + "/" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		if w, ok := wlIndex[kind+"/"+ref.ns+"/"+name]; ok {
			out = append(out, w.owner())
			continue
		}
		out = append(out, auditOwner{Kind: kind, Name: name, APIVersion: ref.apiVersion})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// coveringPDBs returns the PDBs matching a pod template's labels. Using the
// template instead of live pods keeps the result stable when a pod is down.
func coveringPDBs(nsPDBs []*auditPDB, template labels.Set) []string {
	out := []string{}
	for _, p := range nsPDBs {
		if p.selector.Matches(template) {
			out = append(out, p.Name)
		}
	}
	return out
}

// imageInstances finds data services that no operator manages, by image name.
// Only StatefulSets can be quorum systems.
func imageInstances(workloads []*workload, byNS map[string][]*auditPDB, quorumImages []string) []auditInstance {
	keywords := slices.Concat(cacheEngines, quorumImages)
	var out []auditInstance
	for _, w := range workloads {
		if w.controller != nil {
			continue
		}
		engine := detectEngine(w.images, keywords)
		if engine == "" {
			continue
		}
		quorum := w.kind == "StatefulSet" && slices.Contains(quorumImages, engine)
		if !quorum && !slices.Contains(cacheEngines, engine) {
			continue
		}
		out = append(out, auditInstance{
			Namespace: w.ns, Name: w.name, Engine: engine, Kind: w.kind,
			Replicas: int64(w.replicas), Quorum: quorum, Source: "image",
			PDBs: coveringPDBs(byNS[w.ns], w.template), Images: w.images, Ignore: w.ignore,
		})
	}
	return out
}

// detectEngine returns the longest keyword in an image name, so
// "redis-sentinel" wins over "redis". Exporter images are skipped.
func detectEngine(images, keywords []string) string {
	best := ""
	for _, img := range images {
		name := imageName(img)
		if strings.Contains(name, "exporter") {
			continue
		}
		for _, kw := range keywords {
			if len(kw) > len(best) && strings.Contains(name, kw) {
				best = kw
			}
		}
	}
	return best
}

// imageName turns "reg:5000/bitnami/redis:7.2" into "redis".
func imageName(image string) string {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	image, _, _ = strings.Cut(image, ":")
	return strings.ToLower(image)
}

func nestedInt(obj map[string]any, def int64, fields ...string) int64 {
	v, found, _ := unstructured.NestedFieldNoCopy(obj, fields...)
	if !found {
		return def
	}
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return def
}

// --- Teams ---

func handleListTeams(ctx context.Context, c *k8s.Client, _ args) (any, error) {
	type project struct {
		Name     string `json:"name"`
		EnvLabel string `json:"envLabel"`
	}
	type team struct {
		Team     string    `json:"team"`
		Admins   []string  `json:"admins"`
		Projects []project `json:"projects"`
	}
	teams := []team{}
	installed, err := c.ListCustom(ctx, "team.snappcloud.io", "teams", []string{"v1alpha1"},
		func(o *unstructured.Unstructured) {
			t := team{Team: o.GetName(), Admins: []string{}, Projects: []project{}}
			admins, _, _ := unstructured.NestedSlice(o.Object, "spec", "teamAdmins")
			for _, raw := range admins {
				if m, ok := raw.(map[string]any); ok {
					if name, _ := m["name"].(string); name != "" {
						t.Admins = append(t.Admins, name)
					}
				}
			}
			projects, _, _ := unstructured.NestedSlice(o.Object, "spec", "projects")
			for _, raw := range projects {
				if m, ok := raw.(map[string]any); ok {
					name, _ := m["name"].(string)
					env, _ := m["envLabel"].(string)
					t.Projects = append(t.Projects, project{Name: name, EnvLabel: env})
				}
			}
			teams = append(teams, t)
		})
	if err != nil {
		return nil, err
	}
	if !installed {
		return nil, fmt.Errorf("teams.team.snappcloud.io is not served on this cluster (team-operator not installed)")
	}
	sort.Slice(teams, func(i, j int) bool { return teams[i].Team < teams[j].Team })
	return map[string]any{"count": len(teams), "teams": teams}, nil
}
