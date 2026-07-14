package mcp

import (
	"context"
	"fmt"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

func handleListRBAC(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")

	sas, err := c.Clientset.CoreV1().ServiceAccounts(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list serviceaccounts in %s: %w", ns, err)
	}
	accounts := make([]map[string]any, 0, len(sas.Items))
	for i := range sas.Items {
		sa := &sas.Items[i]
		accounts = append(accounts, map[string]any{
			"name": sa.Name, "age": age(sa.CreationTimestamp.Time),
		})
	}

	rbs, err := c.Clientset.RbacV1().RoleBindings(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list rolebindings in %s: %w", ns, err)
	}
	bindings := make([]map[string]any, 0, len(rbs.Items))
	for i := range rbs.Items {
		rb := &rbs.Items[i]
		bindings = append(bindings, map[string]any{
			"name":     rb.Name,
			"grants":   fmt.Sprintf("%s/%s", rb.RoleRef.Kind, rb.RoleRef.Name),
			"subjects": subjectTexts(rb.Subjects),
			"age":      age(rb.CreationTimestamp.Time),
		})
	}

	roles, err := c.Clientset.RbacV1().Roles(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list roles in %s: %w", ns, err)
	}
	roleOut := make([]map[string]any, 0, len(roles.Items))
	for i := range roles.Items {
		r := &roles.Items[i]
		roleOut = append(roleOut, map[string]any{
			"name":  r.Name,
			"rules": ruleTexts(r.Rules),
		})
	}

	return map[string]any{
		"namespace":        ns,
		"service_accounts": accounts,
		"role_bindings":    bindings,
		"roles":            roleOut,
		"note": "Namespace-scoped view only. A ClusterRoleBinding may grant these subjects more than is shown here, " +
			"and this describes bindings rather than deciding access — a definitive check needs SubjectAccessReview, which this read-only server cannot perform.",
	}, nil
}

func subjectTexts(subjects []rbacv1.Subject) []string {
	out := make([]string, 0, len(subjects))
	for _, s := range subjects {
		if s.Kind == "ServiceAccount" && s.Namespace != "" {
			out = append(out, fmt.Sprintf("ServiceAccount/%s/%s", s.Namespace, s.Name))
			continue
		}
		out = append(out, fmt.Sprintf("%s/%s", s.Kind, s.Name))
	}
	return out
}

func ruleTexts(rules []rbacv1.PolicyRule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		groups := strings.Join(r.APIGroups, ",")
		if groups == "" {
			groups = "core"
		}
		text := fmt.Sprintf("%s on %s/%s",
			strings.Join(r.Verbs, ","), groups, strings.Join(r.Resources, ","))
		if len(r.ResourceNames) > 0 {
			text += " (names: " + strings.Join(r.ResourceNames, ",") + ")"
		}
		if len(r.NonResourceURLs) > 0 {
			text += " (urls: " + strings.Join(r.NonResourceURLs, ",") + ")"
		}
		out = append(out, text)
	}
	return out
}
