package debugpod

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func testConfig() Config {
	return Config{Enabled: true, Image: "netshoot:1", CPU: resource.MustParse("200m"), Memory: resource.MustParse("256Mi"),
		Lifetime: 15 * time.Minute, TTL: 30 * time.Second, StartTimeout: 3 * time.Second, ExecTimeout: 10 * time.Second, MaxOutput: 1024}
}

// fakeRunner returns a runner over a fake cluster, recording who it acted as.
func fakeRunner(t *testing.T, cs *fake.Clientset) (*Runner, *[]string) {
	t.Helper()
	var as []string
	r := &Runner{cfg: testConfig(), groups: newGroupCache(func(context.Context) ([]byte, error) {
		return []byte(`{"items":[{"metadata":{"name":"cluster-admins"},"users":["ali@snapp.cab"]}]}`), nil
	}, time.Minute)}
	r.clientFor = func(user string, groups []string) (kubernetes.Interface, *rest.Config, error) {
		as = append(as, user+"|"+strings.Join(groups, ","))
		return cs, &rest.Config{}, nil
	}
	r.exec = func(_ context.Context, _ *rest.Config, _ kubernetes.Interface, _, _ string, cmd []string, out *limitBuffer) error {
		_, _ = out.Write([]byte(strings.Join(cmd, " ")))
		return nil
	}
	return r, &as
}

// runPodsForJobs makes the fake behave like the Job controller: a created Job
// gets a running pod.
func runPodsForJobs(cs *fake.Clientset, phase corev1.PodPhase) {
	cs.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		job := a.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		job.Name, job.Namespace = "snappcloud-debug-abcde", a.GetNamespace()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x1", Namespace: a.GetNamespace(),
				Labels:      map[string]string{"job-name": job.Name, LabelDebug: "true"},
				Annotations: job.Spec.Template.Annotations},
			Spec:   corev1.PodSpec{NodeName: "worker-1"},
			Status: corev1.PodStatus{Phase: phase},
		}
		_ = cs.Tracker().Add(pod)
		_ = cs.Tracker().Add(job)
		return true, job, nil
	})
}

func TestStartCreatesAJobThatCleansUpAfterItself(t *testing.T) {
	cs := fake.NewSimpleClientset()
	runPodsForJobs(cs, corev1.PodRunning)
	r, as := fakeRunner(t, cs)
	got, err := r.Start(context.Background(), "ali@snapp.cab", "team-a", "check DNS from team-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Pod != "snappcloud-debug-abcde-x1" || got.Node != "worker-1" {
		t.Fatalf("started %+v", got)
	}
	if (*as)[0] != "ali@snapp.cab|cluster-admins,system:authenticated,system:authenticated:oauth" {
		t.Fatalf("impersonated %q", (*as)[0])
	}
	job, err := cs.BatchV1().Jobs("team-a").Get(context.Background(), "snappcloud-debug-abcde", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := job.Spec
	if *s.TTLSecondsAfterFinished != 30 || *s.ActiveDeadlineSeconds != 900 || *s.BackoffLimit != 0 {
		t.Fatalf("cleanup settings: ttl=%d deadline=%d backoff=%d", *s.TTLSecondsAfterFinished, *s.ActiveDeadlineSeconds, *s.BackoffLimit)
	}
	p := s.Template.Spec
	c := p.Containers[0]
	if *p.AutomountServiceAccountToken || p.RestartPolicy != corev1.RestartPolicyNever ||
		*c.SecurityContext.AllowPrivilegeEscalation || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("pod is not locked down: %+v", p)
	}
	if !c.Resources.Limits.Cpu().Equal(resource.MustParse("200m")) || !c.Resources.Requests.Memory().Equal(resource.MustParse("256Mi")) {
		t.Fatalf("resources %+v", c.Resources)
	}
	if job.Annotations[AnnotationRequester] != "ali@snapp.cab" {
		t.Fatalf("requester not recorded: %v", job.Annotations)
	}
}

// A pod the scheduler cannot place is reported with its reason, not after
// the whole timeout.
func TestStartReportsWhyThePodIsNotRunning(t *testing.T) {
	cs := fake.NewSimpleClientset()
	runPodsForJobs(cs, corev1.PodPending)
	_ = cs.Tracker().Add(&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "team-a"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "snappcloud-debug-abcde-x1"},
		Reason:         "FailedScheduling", Message: "0/9 nodes are available: insufficient cpu"})
	r, _ := fakeRunner(t, cs)
	start := time.Now()
	_, err := r.Start(context.Background(), "ali@snapp.cab", "team-a", "x")
	if err == nil || !strings.Contains(err.Error(), "insufficient cpu") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("waited out the timeout instead of reporting the reason")
	}
}

func TestOnlyPeopleAreImpersonated(t *testing.T) {
	r, _ := fakeRunner(t, fake.NewSimpleClientset())
	for _, u := range []string{"", "system:admin", "system:serviceaccount:a:b", "kube:admin"} {
		if _, err := r.Start(context.Background(), u, "team-a", "x"); err == nil {
			t.Errorf("%q was impersonated", u)
		}
	}
}

// Exec and finish reach only a running debug pod the same caller started.
func TestExecOnlyIntoOwnDebugPod(t *testing.T) {
	pod := func(name, requester string, debug bool) *corev1.Pod {
		l := map[string]string{}
		if debug {
			l[LabelDebug] = "true"
		}
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Labels: l,
			Annotations: map[string]string{AnnotationRequester: requester}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	}
	cs := fake.NewSimpleClientset(pod("mine", "ali@snapp.cab", true), pod("theirs", "sara@snapp.cab", true),
		pod("app", "ali@snapp.cab", false))
	r, _ := fakeRunner(t, cs)
	ctx := context.Background()
	out, err := r.Exec(ctx, "ali@snapp.cab", "team-a", "mine", "dig +short x", 0)
	if err != nil || !strings.Contains(out, "timeout 10 sh -c") || !strings.HasSuffix(out, "dig +short x") {
		t.Fatalf("exec: %q %v", out, err)
	}
	for _, p := range []string{"theirs", "app"} {
		if _, err := r.Exec(ctx, "ali@snapp.cab", "team-a", p, "id", 0); err == nil {
			t.Errorf("exec into %s allowed", p)
		}
		if err := r.Finish(ctx, "ali@snapp.cab", "team-a", p); err == nil {
			t.Errorf("finish of %s allowed", p)
		}
	}
	if err := r.Finish(ctx, "ali@snapp.cab", "team-a", "mine"); err != nil {
		t.Fatal(err)
	}
}

func TestOutputIsCapped(t *testing.T) {
	b := &limitBuffer{max: 4}
	_, _ = b.Write([]byte("abcdefgh"))
	if got := b.String(); !strings.HasPrefix(got, "abcd\n[output truncated: 4 more bytes") {
		t.Fatalf("got %q", got)
	}
}

func TestConfigNeedsAnImage(t *testing.T) {
	t.Setenv("DEBUG_POD_ENABLED", "true")
	t.Setenv("DEBUG_POD_IMAGE", "")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("enabled without an image")
	}
}
