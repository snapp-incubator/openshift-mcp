// Package debugpod runs short-lived debug pods in a tenant namespace as the
// caller. Every API call impersonates the caller, so the caller's own RBAC
// decides what is allowed and the audit log names them.
//
// A debug pod is a Job: the caller needs create on jobs and pods/exec, never
// delete. The pod exits when Finish touches its done file or when the Job's
// activeDeadlineSeconds passes, and the Job controller deletes Job and pod
// ttlSecondsAfterFinished later.
package debugpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	// LabelDebug marks a debug pod's Job and pod.
	LabelDebug = "snappcloud.io/debug-pod"
	// AnnotationRequester names who asked for the pod; exec and finish accept
	// only that caller.
	AnnotationRequester = "snappcloud.io/requested-by"
	// AnnotationReason is why it was started.
	AnnotationReason = "snappcloud.io/reason"

	container = "debug"
	doneFile  = "/tmp/done"
)

// implicitGroups are the groups the API server adds to every OAuth user.
var implicitGroups = []string{"system:authenticated", "system:authenticated:oauth"}

// Config is the debug pod's shape, from the environment.
type Config struct {
	Enabled       bool
	Image         string
	CPU           resource.Quantity
	Memory        resource.Quantity
	Lifetime      time.Duration // activeDeadlineSeconds
	TTL           time.Duration // ttlSecondsAfterFinished
	StartTimeout  time.Duration
	ExecTimeout   time.Duration
	MaxOutput     int
	PriorityClass string
}

// ConfigFromEnv reads DEBUG_POD_* with defaults. Enabled needs an image.
func ConfigFromEnv() (Config, error) {
	c := Config{
		Enabled:       os.Getenv("DEBUG_POD_ENABLED") == "true",
		Image:         os.Getenv("DEBUG_POD_IMAGE"),
		CPU:           resource.MustParse("200m"),
		Memory:        resource.MustParse("256Mi"),
		Lifetime:      15 * time.Minute,
		TTL:           30 * time.Second,
		StartTimeout:  60 * time.Second,
		ExecTimeout:   60 * time.Second,
		MaxOutput:     64 << 10,
		PriorityClass: os.Getenv("DEBUG_POD_PRIORITY_CLASS"),
	}
	var errs []error
	quantity := func(env string, dst *resource.Quantity) {
		if v := os.Getenv(env); v != "" {
			q, err := resource.ParseQuantity(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", env, err))
				return
			}
			*dst = q
		}
	}
	duration := func(env string, dst *time.Duration) {
		if v := os.Getenv(env); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				errs = append(errs, fmt.Errorf("%s: invalid duration %q", env, v))
				return
			}
			*dst = d
		}
	}
	quantity("DEBUG_POD_CPU", &c.CPU)
	quantity("DEBUG_POD_MEMORY", &c.Memory)
	duration("DEBUG_POD_LIFETIME", &c.Lifetime)
	duration("DEBUG_POD_TTL", &c.TTL)
	duration("DEBUG_POD_START_TIMEOUT", &c.StartTimeout)
	duration("DEBUG_POD_EXEC_TIMEOUT", &c.ExecTimeout)
	if v := os.Getenv("DEBUG_POD_MAX_OUTPUT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("DEBUG_POD_MAX_OUTPUT: invalid %q", v))
		} else {
			c.MaxOutput = n
		}
	}
	if c.Enabled && c.Image == "" {
		errs = append(errs, errors.New("DEBUG_POD_IMAGE is required when DEBUG_POD_ENABLED=true"))
	}
	return c, errors.Join(errs...)
}

// Runner starts, drives and finishes debug pods.
type Runner struct {
	cfg  Config
	base *rest.Config
	// groups lists OpenShift Group objects with the server's own access, to
	// impersonate the caller with the groups their access comes from.
	groups *groupCache
	// clientFor builds a client acting as the caller. Replaced in tests.
	clientFor func(user string, groups []string) (kubernetes.Interface, *rest.Config, error)
	exec      func(ctx context.Context, cfg *rest.Config, cs kubernetes.Interface, ns, pod string, cmd []string, out *limitBuffer) error
}

// New builds a Runner over the server's own config. cs lists groups.
func New(cfg Config, base *rest.Config, cs kubernetes.Interface) *Runner {
	r := &Runner{cfg: cfg, base: base, groups: newGroupCache(func(ctx context.Context) ([]byte, error) {
		return cs.CoreV1().RESTClient().Get().AbsPath("/apis/user.openshift.io/v1/groups").DoRaw(ctx)
	}, 5*time.Minute)}
	r.clientFor = r.impersonating
	r.exec = remoteExec
	return r
}

func (r *Runner) impersonating(user string, groups []string) (kubernetes.Interface, *rest.Config, error) {
	cfg := rest.CopyConfig(r.base)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
	cs, err := kubernetes.NewForConfig(cfg)
	return cs, cfg, err
}

// caller returns a client acting as user. ServiceAccounts and system users
// are refused: the server may only stand in for people.
func (r *Runner) caller(ctx context.Context, user string) (kubernetes.Interface, *rest.Config, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return nil, nil, errors.New("no caller identity: a debug pod runs as the person who asked for it")
	}
	if strings.HasPrefix(user, "system:") || user == "kube:admin" {
		return nil, nil, fmt.Errorf("%s is not a person; debug pods run only as a user", user)
	}
	groups := append(r.groups.groupsFor(ctx, user), implicitGroups...)
	return r.clientFor(user, groups)
}

// Started is what Start returns.
type Started struct {
	Namespace string    `json:"namespace"`
	Job       string    `json:"job"`
	Pod       string    `json:"pod"`
	Node      string    `json:"node,omitempty"`
	Image     string    `json:"image"`
	ExpiresAt time.Time `json:"expires_at"`
	Note      string    `json:"note"`
}

// Start creates the Job as user and waits for its pod to run. A pod that
// cannot be placed is reported with the reason as soon as one is known.
func (r *Runner) Start(ctx context.Context, user, ns, reason string) (*Started, error) {
	cs, _, err := r.caller(ctx, user)
	if err != nil {
		return nil, err
	}
	job, err := cs.BatchV1().Jobs(ns).Create(ctx, r.job(user, reason), metav1.CreateOptions{})
	if err != nil {
		return nil, explain(err, "create a Job in "+ns)
	}
	wctx, cancel := context.WithTimeout(ctx, r.cfg.StartTimeout)
	defer cancel()
	pod, err := r.waitRunning(wctx, cs, ns, job.Name)
	if err != nil {
		return nil, fmt.Errorf("debug pod for job %s did not start: %w. It is removed automatically within %s",
			job.Name, err, r.cfg.Lifetime+r.cfg.TTL)
	}
	return &Started{Namespace: ns, Job: job.Name, Pod: pod.Name, Node: pod.Spec.NodeName, Image: r.cfg.Image,
		ExpiresAt: job.CreationTimestamp.Add(r.cfg.Lifetime),
		Note: "Run commands with debug_pod_exec. Call debug_pod_finish when done: the pod then exits and is deleted " +
			"automatically. It has no extra capabilities (no NET_RAW: ping and tcpdump do not work; use curl, dig, nc)."}, nil
}

func (r *Runner) job(user, reason string) *batchv1.Job {
	no, yes := false, true
	deadline := int64(r.cfg.Lifetime.Seconds())
	ttl := int32(r.cfg.TTL.Seconds())
	backoff := int32(0)
	labels := map[string]string{LabelDebug: "true", "app.kubernetes.io/managed-by": "snappcloud-bot"}
	ann := map[string]string{AnnotationRequester: user, AnnotationReason: truncate(reason, 500)}
	res := corev1.ResourceList{corev1.ResourceCPU: r.cfg.CPU, corev1.ResourceMemory: r.cfg.Memory}
	// Exits 0 once the done file appears, and on SIGTERM at the deadline.
	script := "trap 'exit 0' TERM INT; while [ ! -e " + doneFile + " ]; do sleep 1; done"
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "snappcloud-debug-", Labels: labels, Annotations: ann},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			BackoffLimit:            &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: ann},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					AutomountServiceAccountToken:  &no,
					EnableServiceLinks:            &no,
					TerminationGracePeriodSeconds: ptr(int64(1)),
					PriorityClassName:             r.cfg.PriorityClass,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &yes,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            container,
						Image:           r.cfg.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"sh", "-c", script},
						Resources:       corev1.ResourceRequirements{Requests: res, Limits: res},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
}

// waitRunning polls the Job's pod until it runs, returning early with the
// reason when it cannot be created, placed or pulled.
func (r *Runner) waitRunning(ctx context.Context, cs kubernetes.Interface, ns, job string) (*corev1.Pod, error) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job})
		if err == nil && len(pods.Items) > 0 {
			p := &pods.Items[0]
			switch p.Status.Phase {
			case corev1.PodRunning:
				return p, nil
			case corev1.PodFailed, corev1.PodSucceeded:
				return nil, fmt.Errorf("pod %s ended (%s) before it could be used", p.Name, p.Status.Phase)
			}
			if why := waitingReason(p); why != "" {
				return nil, errors.New(why)
			}
			if why := r.events(ctx, cs, ns, "Pod", p.Name, "FailedScheduling", "FailedCreatePodSandBox", "FailedMount"); why != "" {
				return nil, errors.New(why)
			}
		}
		if why := r.events(ctx, cs, ns, "Job", job, "FailedCreate"); why != "" {
			return nil, errors.New(why)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("not running after %s", r.cfg.StartTimeout)
		case <-tick.C:
		}
	}
}

func waitingReason(p *corev1.Pod) string {
	for _, s := range p.Status.ContainerStatuses {
		if w := s.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
				return w.Reason + ": " + w.Message
			}
		}
	}
	return ""
}

func (r *Runner) events(ctx context.Context, cs kubernetes.Interface, ns, kind, name string, reasons ...string) string {
	evs, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=" + kind + ",involvedObject.name=" + name})
	if err != nil {
		return ""
	}
	for _, e := range evs.Items {
		for _, want := range reasons {
			if e.Reason == want {
				return e.Reason + ": " + e.Message
			}
		}
	}
	return ""
}

// Exec runs one shell command in a debug pod the caller started, as the caller.
func (r *Runner) Exec(ctx context.Context, user, ns, pod, command string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", errors.New("command is empty")
	}
	if timeout <= 0 || timeout > r.cfg.ExecTimeout {
		timeout = r.cfg.ExecTimeout
	}
	cs, cfg, err := r.caller(ctx, user)
	if err != nil {
		return "", err
	}
	if err := r.owned(ctx, cs, user, ns, pod); err != nil {
		return "", err
	}
	secs := strconv.Itoa(int(timeout.Seconds()))
	// timeout(1) stops the command in the pod, not only the stream here.
	cmd := []string{"sh", "-c",
		`if command -v timeout >/dev/null 2>&1; then exec timeout ` + secs + ` sh -c "$1"; else exec sh -c "$1"; fi`,
		"sh", command}
	ectx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	out := &limitBuffer{max: r.cfg.MaxOutput}
	eerr := r.exec(ectx, cfg, cs, ns, pod, cmd, out)
	text := out.String()
	if eerr != nil {
		if ectx.Err() != nil {
			return text, fmt.Errorf("timed out after %s; output so far:\n%s", timeout, text)
		}
		// A non-zero exit is a result, not a failure of the tool.
		return text + "\n[exit: " + eerr.Error() + "]", nil
	}
	return text, nil
}

// Finish makes the pod exit; the Job controller then deletes it.
func (r *Runner) Finish(ctx context.Context, user, ns, pod string) error {
	cs, cfg, err := r.caller(ctx, user)
	if err != nil {
		return err
	}
	if err := r.owned(ctx, cs, user, ns, pod); err != nil {
		return err
	}
	ectx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return r.exec(ectx, cfg, cs, ns, pod, []string{"touch", doneFile}, &limitBuffer{max: 1024})
}

// owned checks the pod is a running debug pod this caller started: the tools
// never exec into anything else, even where the caller could.
func (r *Runner) owned(ctx context.Context, cs kubernetes.Interface, user, ns, pod string) error {
	p, err := cs.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return explain(err, "read pod "+ns+"/"+pod)
	}
	if p.Labels[LabelDebug] != "true" || p.Annotations[AnnotationRequester] != user {
		return fmt.Errorf("%s/%s is not a debug pod you started", ns, pod)
	}
	if p.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("debug pod %s is %s, not running; start a new one", pod, p.Status.Phase)
	}
	return nil
}

// List returns the caller's debug pods in a namespace.
func (r *Runner) List(ctx context.Context, user, ns string) ([]map[string]any, error) {
	cs, _, err := r.caller(ctx, user)
	if err != nil {
		return nil, err
	}
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelDebug + "=true"})
	if err != nil {
		return nil, explain(err, "list pods in "+ns)
	}
	var out []map[string]any
	for _, p := range pods.Items {
		if p.Annotations[AnnotationRequester] != user {
			continue
		}
		out = append(out, map[string]any{"pod": p.Name, "job": p.Labels["job-name"], "phase": string(p.Status.Phase),
			"node": p.Spec.NodeName, "started": p.CreationTimestamp.Time, "reason": p.Annotations[AnnotationReason]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["pod"].(string) < out[j]["pod"].(string) })
	return out, nil
}

func remoteExec(ctx context.Context, cfg *rest.Config, cs kubernetes.Interface, ns, pod string, cmd []string, out *limitBuffer) error {
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: cmd, Stdout: true, Stderr: true},
			scheme.ParameterCodec)
	spdy, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return err
	}
	ws, err := remotecommand.NewWebSocketExecutor(cfg, "GET", req.URL().String())
	if err != nil {
		return err
	}
	ex, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return err
	}
	err = ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: out, Stderr: out})
	if err != nil && apierrors.IsForbidden(err) {
		return explain(err, "exec into "+ns+"/"+pod)
	}
	return err
}

// explain turns an RBAC refusal into what the caller lacks.
func explain(err error, what string) error {
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("your access does not allow you to %s (%v). A debug pod needs create on jobs and "+
			"pods/exec in the namespace", what, err)
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsInvalid(err) {
		return fmt.Errorf("%s: %w", what, err)
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) && strings.Contains(strings.ToLower(status.Status().Message), "quota") {
		return fmt.Errorf("the namespace quota does not leave room for a debug pod: %w", err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// limitBuffer keeps the first max bytes written and counts the rest.
type limitBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	max     int
	dropped int
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	room := b.max - b.buf.Len()
	if room <= 0 {
		b.dropped += len(p)
		return len(p), nil
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		b.dropped += len(p) - room
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dropped > 0 {
		return b.buf.String() + fmt.Sprintf("\n[output truncated: %d more bytes; narrow the command]", b.dropped)
	}
	return b.buf.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func ptr[T any](v T) *T { return &v }

// groupCache maps users to their OpenShift groups, refreshed every ttl.
type groupCache struct {
	fetch   func(context.Context) ([]byte, error)
	ttl     time.Duration
	mu      sync.Mutex
	expires time.Time
	byUser  map[string][]string
}

func newGroupCache(fetch func(context.Context) ([]byte, error), ttl time.Duration) *groupCache {
	return &groupCache{fetch: fetch, ttl: ttl, byUser: map[string][]string{}}
}

func (g *groupCache) groupsFor(ctx context.Context, user string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().After(g.expires) {
		if data, err := g.fetch(ctx); err == nil {
			g.byUser = parseGroups(data)
		}
		g.expires = time.Now().Add(g.ttl)
	}
	return append([]string(nil), g.byUser[user]...)
}

func parseGroups(data []byte) map[string][]string {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Users []string `json:"users"`
		} `json:"items"`
	}
	m := map[string][]string{}
	if json.Unmarshal(data, &list) != nil {
		return m
	}
	for _, grp := range list.Items {
		for _, u := range grp.Users {
			m[u] = append(m[u], grp.Metadata.Name)
		}
	}
	return m
}
