package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

func handleListNetworkPolicies(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	list, err := c.Clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{
		Limit: defaultListLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list networkpolicies in %s: %w", ns, err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		np := &list.Items[i]
		types := make([]string, 0, len(np.Spec.PolicyTypes))
		for _, t := range np.Spec.PolicyTypes {
			types = append(types, string(t))
		}
		entry := map[string]any{
			"name": np.Name, "namespace": ns,
			"applies_to":   selectorText(&np.Spec.PodSelector),
			"policy_types": types,
			"age":          age(np.CreationTimestamp.Time),
		}
		if hasPolicyType(np, networkingv1.PolicyTypeIngress) {
			entry["ingress"] = ingressRules(np)
		}
		if hasPolicyType(np, networkingv1.PolicyTypeEgress) {
			entry["egress"] = egressRules(np)
		}
		out = append(out, entry)
	}
	return map[string]any{
		"namespace": ns, "count": len(out), "network_policies": out,
		"note": "A pod selected by any policy is default-deny for that policy's types. " +
			"'DENY ALL' means the type is enforced with no rules permitting traffic. " +
			"No policies at all means all traffic is allowed.",
	}, nil
}

func handleListIngresses(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	list, err := c.Clientset.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{
		Limit: defaultListLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list ingresses in %s: %w", ns, err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		ing := &list.Items[i]
		entry := map[string]any{
			"name": ing.Name, "namespace": ns,
			"rules": ingressRuleTexts(ing),
			"age":   age(ing.CreationTimestamp.Time),
		}
		if ing.Spec.IngressClassName != nil {
			entry["class"] = *ing.Spec.IngressClassName
		}
		var tlsHosts []string
		for _, tls := range ing.Spec.TLS {
			tlsHosts = append(tlsHosts, tls.Hosts...)
		}
		entry["tls_hosts"] = tlsHosts

		var addresses []string
		for _, lb := range ing.Status.LoadBalancer.Ingress {
			if lb.Hostname != "" {
				addresses = append(addresses, lb.Hostname)
			}
			if lb.IP != "" {
				addresses = append(addresses, lb.IP)
			}
		}
		entry["addresses"] = addresses
		if len(addresses) == 0 {
			entry["warning"] = "no address assigned: no ingress controller has claimed this Ingress (check the class)"
		}
		out = append(out, entry)
	}
	return map[string]any{"namespace": ns, "count": len(out), "ingresses": out}, nil
}

func ingressRuleTexts(ing *networkingv1.Ingress) []string {
	var out []string
	for _, rule := range ing.Spec.Rules {
		host := rule.Host
		if host == "" {
			host = "*"
		}
		if rule.HTTP == nil {
			out = append(out, host)
			continue
		}
		for _, p := range rule.HTTP.Paths {
			backend := "<none>"
			if svc := p.Backend.Service; svc != nil {
				port := ""
				if svc.Port.Name != "" {
					port = svc.Port.Name
				} else if svc.Port.Number != 0 {
					port = fmt.Sprint(svc.Port.Number)
				}
				backend = svc.Name + ":" + port
			} else if res := p.Backend.Resource; res != nil {
				backend = res.Kind + "/" + res.Name
			}
			out = append(out, fmt.Sprintf("%s%s -> %s", host, p.Path, backend))
		}
	}
	return out
}

func hasPolicyType(np *networkingv1.NetworkPolicy, want networkingv1.PolicyType) bool {
	if len(np.Spec.PolicyTypes) == 0 {
		if want == networkingv1.PolicyTypeIngress {
			return true
		}
		return len(np.Spec.Egress) > 0
	}
	for _, t := range np.Spec.PolicyTypes {
		if t == want {
			return true
		}
	}
	return false
}

func ingressRules(np *networkingv1.NetworkPolicy) []string {
	if len(np.Spec.Ingress) == 0 {
		return []string{"DENY ALL (ingress enforced, no rules)"}
	}
	out := make([]string, 0, len(np.Spec.Ingress))
	for _, rule := range np.Spec.Ingress {
		from := "ALL sources"
		if len(rule.From) > 0 {
			from = strings.Join(peerTexts(rule.From), " OR ")
		}
		out = append(out, fmt.Sprintf("allow from %s on %s", from, portTexts(rule.Ports)))
	}
	return out
}

func egressRules(np *networkingv1.NetworkPolicy) []string {
	if len(np.Spec.Egress) == 0 {
		return []string{"DENY ALL (egress enforced, no rules)"}
	}
	out := make([]string, 0, len(np.Spec.Egress))
	for _, rule := range np.Spec.Egress {
		to := "ALL destinations"
		if len(rule.To) > 0 {
			to = strings.Join(peerTexts(rule.To), " OR ")
		}
		out = append(out, fmt.Sprintf("allow to %s on %s", to, portTexts(rule.Ports)))
	}
	return out
}

func peerTexts(peers []networkingv1.NetworkPolicyPeer) []string {
	out := make([]string, 0, len(peers))
	for i := range peers {
		p := &peers[i]
		var parts []string
		if p.NamespaceSelector != nil {
			parts = append(parts, "namespaces("+selectorText(p.NamespaceSelector)+")")
		}
		if p.PodSelector != nil {
			parts = append(parts, "pods("+selectorText(p.PodSelector)+")")
		}
		if p.IPBlock != nil {
			ip := p.IPBlock.CIDR
			if len(p.IPBlock.Except) > 0 {
				ip += " except " + strings.Join(p.IPBlock.Except, ",")
			}
			parts = append(parts, "ipBlock("+ip+")")
		}
		if len(parts) == 0 {
			continue
		}
		out = append(out, strings.Join(parts, " AND "))
	}
	return out
}

func portTexts(ports []networkingv1.NetworkPolicyPort) string {
	if len(ports) == 0 {
		return "ALL ports"
	}
	out := make([]string, 0, len(ports))
	for i := range ports {
		p := &ports[i]
		proto := "TCP"
		if p.Protocol != nil {
			proto = string(*p.Protocol)
		}
		switch {
		case p.Port == nil:
			out = append(out, "ALL/"+proto)
		case p.EndPort != nil:
			out = append(out, fmt.Sprintf("%s-%d/%s", p.Port.String(), *p.EndPort, proto))
		default:
			out = append(out, p.Port.String()+"/"+proto)
		}
	}
	return strings.Join(out, ",")
}

func selectorText(sel *metav1.LabelSelector) string {
	if sel == nil {
		return "<none>"
	}
	if len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
		return "ALL pods in namespace"
	}
	parts := make([]string, 0, len(sel.MatchLabels)+len(sel.MatchExpressions))
	for k, v := range sel.MatchLabels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	for _, e := range sel.MatchExpressions {
		parts = append(parts, fmt.Sprintf("%s %s %v", e.Key, strings.ToLower(string(e.Operator)), e.Values))
	}
	return strings.Join(parts, ",")
}
