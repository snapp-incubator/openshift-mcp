package mcp

import (
	"context"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func ptr[T any](v T) *T { return &v }

func TestListConfigExposesKeysButNeverSecretValues(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-creds", Namespace: "team-a"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"password": []byte("hunter2-super-secret"),
			"username": []byte("admin-user-value"),
		},
	}
	session := connectWith(t, fake.NewSimpleClientset(secret))
	text := call(t, session, "list_config", map[string]any{"namespace": "team-a"})

	requireContains(t, "list_config", text, "db-creds", "password", "username", "Opaque")

	for _, leak := range []string{
		"hunter2-super-secret",
		"admin-user-value",
		"aHVudGVyMi1zdXBlci1zZWNyZXQ=",
		"YWRtaW4tdXNlci12YWx1ZQ==",
	} {
		if strings.Contains(text, leak) {
			t.Errorf("list_config LEAKED secret value %q:\n%s", leak, text)
		}
	}
}

func TestListConfigShowsConfigMapKeysAndSize(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: "team-a"},
		Data:       map[string]string{"LOG_LEVEL": "debug"},
	}
	session := connectWith(t, fake.NewSimpleClientset(cm))
	text := call(t, session, "list_config", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_config", text, "app-config", "LOG_LEVEL", `"total_bytes": 5`)
}

func TestListConfigReturnsBothKinds(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: "team-a"},
		Data:       map[string]string{"LOG_LEVEL": "debug"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-creds", Namespace: "team-a"},
		Data:       map[string][]byte{"password": []byte("x")},
	}
	session := connectWith(t, fake.NewSimpleClientset(cm, secret))
	text := call(t, session, "list_config", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_config", text, "app-config", "db-creds")
}

func TestListNetworkPoliciesFlagsDenyAll(t *testing.T) {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default-deny", Namespace: "team-a"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	session := connectWith(t, fake.NewSimpleClientset(np))
	text := call(t, session, "list_network_policies", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_network_policies", text,
		"default-deny", "ALL pods in namespace", "DENY ALL")
}

func TestListNetworkPoliciesRendersRules(t *testing.T) {
	tcp := corev1.ProtocolTCP
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-web", Namespace: "team-a"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gateway"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{
					Protocol: &tcp,
					Port:     ptr(intstr.FromInt32(8080)),
				}},
			}},
		},
	}
	session := connectWith(t, fake.NewSimpleClientset(np))
	text := call(t, session, "list_network_policies", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_network_policies", text,
		"allow-web", "app=web", "pods(app=gateway)", "8080/TCP")

	if strings.Contains(text, "DENY ALL (ingress enforced") {
		t.Errorf("policy with rules must not be reported as DENY ALL:\n%s", text)
	}
}

func TestListIngressesFlagsMissingAddress(t *testing.T) {
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a"},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr("nginx"),
			Rules: []networkingv1.IngressRule{{
				Host: "web.example.com",
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path: "/api",
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: "api-svc",
									Port: networkingv1.ServiceBackendPort{Number: 8080},
								},
							},
						}},
					},
				},
			}},
		},
	}
	session := connectWith(t, fake.NewSimpleClientset(ing))
	text := call(t, session, "list_ingresses", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_ingresses", text,
		"web.example.com/api -> api-svc:8080", "nginx", "no address assigned")
}

func TestListIngressesWithAddressHasNoWarning(t *testing.T) {
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a"},
		Status: networkingv1.IngressStatus{
			LoadBalancer: networkingv1.IngressLoadBalancerStatus{
				Ingress: []networkingv1.IngressLoadBalancerIngress{{Hostname: "lb.example.com"}},
			},
		},
	}
	session := connectWith(t, fake.NewSimpleClientset(ing))
	text := call(t, session, "list_ingresses", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_ingresses", text, "lb.example.com")
	if strings.Contains(text, "no address assigned") {
		t.Errorf("an Ingress with an address must not be flagged:\n%s", text)
	}
}

func storageClass(name string, isDefault bool) *storagev1.StorageClass {
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: "csi.example.com",
	}
	if isDefault {
		sc.Annotations = map[string]string{defaultClassAnnotation: "true"}
	}
	return sc
}

func pvc(name string, phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
}

func TestListPVCsAttachesStorageClassesWhenUnbound(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset(
		pvc("data", corev1.ClaimPending),
		storageClass("fast", true), storageClass("slow", false)))
	text := call(t, session, "list_pvcs", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_pvcs", text,
		"data", "Pending", `"default_storage_class": "fast"`, "csi.example.com")
}

func TestListPVCsOmitsStorageClassesWhenAllBound(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset(
		pvc("data", corev1.ClaimBound), storageClass("fast", true)))
	text := call(t, session, "list_pvcs", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_pvcs", text, "data", "Bound")
	if strings.Contains(text, "storage_classes") {
		t.Errorf("bound PVCs need no StorageClass context:\n%s", text)
	}
}

func TestListPVCsWarnsWhenNoDefaultStorageClass(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset(
		pvc("data", corev1.ClaimPending), storageClass("only", false)))
	text := call(t, session, "list_pvcs", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_pvcs", text, "No default StorageClass", "stay Pending forever")
}

func TestListPVCsWarnsWhenMultipleDefaultStorageClasses(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset(
		pvc("data", corev1.ClaimPending),
		storageClass("a", true), storageClass("b", true)))
	text := call(t, session, "list_pvcs", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_pvcs", text, "2 StorageClasses are marked default")
}

func TestListPVCsExplainsWaitForFirstConsumer(t *testing.T) {
	mode := storagev1.VolumeBindingWaitForFirstConsumer
	sc := storageClass("wffc", true)
	sc.VolumeBindingMode = &mode
	session := connectWith(t, fake.NewSimpleClientset(pvc("data", corev1.ClaimPending), sc))
	text := call(t, session, "list_pvcs", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_pvcs", text, "WaitForFirstConsumer", "expected, not a fault")
}

func TestListRBACShowsBindingsAndSubjects(t *testing.T) {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "app-sa", Namespace: "team-a"},
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-reader", Namespace: "team-a"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get", "list"},
		}},
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "app-can-read", Namespace: "team-a"},
		RoleRef:    rbacv1.RoleRef{Kind: "Role", Name: "pod-reader"},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: "app-sa", Namespace: "team-a",
		}},
	}
	session := connectWith(t, fake.NewSimpleClientset(sa, role, rb))
	text := call(t, session, "list_rbac", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_rbac", text,
		"app-sa", "app-can-read", "Role/pod-reader",
		"ServiceAccount/team-a/app-sa", "get,list on core/pods",
		"SubjectAccessReview")
}

func TestListHPAsReportsBrokenMetricsAndTargets(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web"},
			MinReplicas:    ptr(int32(2)),
			MaxReplicas:    10,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: ptr(int32(80)),
					},
				},
			}},
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			CurrentReplicas: 2,
			DesiredReplicas: 2,
			Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{{
				Type:    autoscalingv2.ScalingActive,
				Status:  corev1.ConditionFalse,
				Reason:  "FailedGetResourceMetric",
				Message: "unable to get metrics for resource cpu",
			}},
		},
	}
	session := connectWith(t, fake.NewSimpleClientset(hpa))
	text := call(t, session, "list_hpas", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_hpas", text,
		"Deployment/web", "FailedGetResourceMetric", "target 80%", "unable to get metrics")
}

func TestListPDBsFlagsZeroDisruptionsAllowed(t *testing.T) {
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "web-pdb", Namespace: "team-a"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr(intstr.FromInt32(3)),
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		},
		Status: policyv1.PodDisruptionBudgetStatus{
			CurrentHealthy: 3, DesiredHealthy: 3, ExpectedPods: 3, DisruptionsAllowed: 0,
		},
	}
	session := connectWith(t, fake.NewSimpleClientset(pdb))
	text := call(t, session, "list_pdbs", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_pdbs", text, "web-pdb", "app=web", "blocks node drains")
}

func testDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(3))},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 3},
	}
}

func failedJob() *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "team-a"},
		Spec:       batchv1.JobSpec{Completions: ptr(int32(1)), BackoffLimit: ptr(int32(4))},
		Status: batchv1.JobStatus{
			Failed: 5,
			Conditions: []batchv1.JobCondition{{
				Type:    batchv1.JobFailed,
				Status:  corev1.ConditionTrue,
				Reason:  "BackoffLimitExceeded",
				Message: "Job has reached the specified backoff limit",
			}},
		},
	}
}

func suspendedCronJob() *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "team-a"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 2 * * *", Suspend: ptr(true)},
	}
}

func TestListWorkloadsIncludesJobsAndCronJobsByDefault(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset(
		testDeployment(), failedJob(), suspendedCronJob()))
	text := call(t, session, "list_workloads", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_workloads", text,
		"web", "migrate", "BackoffLimitExceeded",
		"nightly", "0 2 * * *", "suspended")
}

func TestListWorkloadsKindsFilter(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset(
		testDeployment(), failedJob(), suspendedCronJob()))
	text := call(t, session, "list_workloads", map[string]any{
		"namespace": "team-a", "kinds": []any{"job"},
	})
	requireContains(t, "list_workloads", text, "migrate")
	for _, excluded := range []string{`"name": "web"`, `"name": "nightly"`} {
		if strings.Contains(text, excluded) {
			t.Errorf("kinds=[job] must not return %s:\n%s", excluded, text)
		}
	}
}

func TestListWorkloadsBackwardCompatibleDefault(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset(testDeployment()))
	text := call(t, session, "list_workloads", map[string]any{"namespace": "team-a"})
	requireContains(t, "list_workloads", text, `"kind": "Deployment"`, `"ready": "3/3"`)
}

func TestGetWorkloadJob(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset(failedJob()))
	text := call(t, session, "get_workload", map[string]any{
		"namespace": "team-a", "kind": "job", "name": "migrate",
	})
	requireContains(t, "get_workload", text,
		"BackoffLimitExceeded", `"backoff_limit": 4`, `"failed": 5`)
}

func TestGetWorkloadCronJobListsActiveJobs(t *testing.T) {
	cj := suspendedCronJob()
	cj.Status.Active = []corev1.ObjectReference{{Kind: "Job", Name: "nightly-28001"}}
	session := connectWith(t, fake.NewSimpleClientset(cj))
	text := call(t, session, "get_workload", map[string]any{
		"namespace": "team-a", "kind": "cronjob", "name": "nightly",
	})
	requireContains(t, "get_workload", text, "nightly-28001", `"suspended": true`, "0 2 * * *")
}

func TestGetWorkloadRejectsUnknownKind(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset())
	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "get_workload",
		Arguments: map[string]any{"namespace": "team-a", "kind": "bogus", "name": "x"},
	})
	if err != nil {
		t.Fatalf("expected tool error, got protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for an unknown kind")
	}
}

func TestListNamespacesFilters(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
			Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other"},
			Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}},
	))
	text := call(t, session, "list_namespaces", map[string]any{"filter": "team"})
	requireContains(t, "list_namespaces", text, "team-a")
	if strings.Contains(text, "other") {
		t.Errorf("filter=team must exclude 'other':\n%s", text)
	}
}

func TestListNamespacesFlagsTerminating(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "stuck"},
			Status: corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating}},
	))
	text := call(t, session, "list_namespaces", nil)
	requireContains(t, "list_namespaces", text, "stuck", "finalizer")
}

func TestDiagnoseNamespaceRanksCriticalFirst(t *testing.T) {
	oomPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "team-a"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "api", Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
				},
			}},
		},
	}
	session := connectWith(t, fake.NewSimpleClientset(testPod(), oomPod))
	text := call(t, session, "diagnose_namespace", map[string]any{"namespace": "team-a"})

	requireContains(t, "diagnose_namespace", text,
		"crash-looping", "previous=true", "OOMKilled")

	crit := strings.Index(text, "crash-looping")
	oom := strings.Index(text, "OOMKilled")
	if crit == -1 || oom == -1 || crit > oom {
		t.Errorf("critical finding must rank before the warning:\n%s", text)
	}
}

func TestDiagnoseNamespaceFlagsExhaustedQuota(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: "team-a"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("10")},
			Used: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("10")},
		},
	}
	session := connectWith(t, fake.NewSimpleClientset(quota))
	text := call(t, session, "diagnose_namespace", map[string]any{"namespace": "team-a"})
	requireContains(t, "diagnose_namespace", text, "exhausted", "resourcequota/compute")
}

func TestDiagnoseNamespaceFlagsServiceWithNoEndpoints(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "nonexistent"},
		},
	}
	session := connectWith(t, fake.NewSimpleClientset(svc))
	text := call(t, session, "diagnose_namespace", map[string]any{"namespace": "team-a"})
	requireContains(t, "diagnose_namespace", text, "service/web", "0 ready endpoints")
}

func TestDiagnoseNamespaceIgnoresSelectorlessService(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "team-a"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "example.com"},
	}
	session := connectWith(t, fake.NewSimpleClientset(svc))
	text := call(t, session, "diagnose_namespace", map[string]any{"namespace": "team-a"})
	if strings.Contains(text, "service/external") {
		t.Errorf("ExternalName service must not be flagged:\n%s", text)
	}
}

func TestDiagnoseNamespaceCleanIsExplicit(t *testing.T) {
	session := connectWith(t, fake.NewSimpleClientset())
	text := call(t, session, "diagnose_namespace", map[string]any{"namespace": "empty"})
	requireContains(t, "diagnose_namespace", text, "No problems detected")
}
