// Package k8s builds the read-only Kubernetes clients the MCP tools query with.
package k8s

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Config holds connection settings.
type Config struct {
	Kubeconfig string
	Context    string
	Timeout    time.Duration
	QPS        float32
	Burst      int
}

// ConfigFromEnv reads settings from the environment with sane defaults.
func ConfigFromEnv() *Config {
	cfg := &Config{Timeout: 30 * time.Second, QPS: 50, Burst: 100}
	if v := os.Getenv("K8S_KUBECONFIG"); v != "" {
		cfg.Kubeconfig = v
	}
	if v := os.Getenv("K8S_CONTEXT"); v != "" {
		cfg.Context = v
	}
	if v := os.Getenv("K8S_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Timeout = d
		}
	}
	if v := os.Getenv("K8S_QPS"); v != "" {
		if f, err := strconv.ParseFloat(v, 32); err == nil && f > 0 {
			cfg.QPS = float32(f)
		}
	}
	if v := os.Getenv("K8S_BURST"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Burst = n
		}
	}
	return cfg
}

// Client bundles the typed and dynamic Kubernetes clients. All tool access is
// read-only; write verbs are never used (and the ServiceAccount's ClusterRole
// must not grant them).
type Client struct {
	Clientset kubernetes.Interface
	Dynamic   dynamic.Interface
}

// New builds the clients: explicit kubeconfig/context when set, then
// in-cluster service account, then the default kubeconfig.
func New(cfg *Config) (*Client, error) {
	if cfg == nil {
		cfg = ConfigFromEnv()
	}
	restCfg, err := restConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build rest config: %w", err)
	}
	restCfg.Timeout = cfg.Timeout
	restCfg.QPS = cfg.QPS
	restCfg.Burst = cfg.Burst

	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	return &Client{Clientset: cs, Dynamic: dyn}, nil
}

func restConfig(cfg *Config) (*rest.Config, error) {
	if cfg.Kubeconfig != "" || cfg.Context != "" {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		rules.ExplicitPath = cfg.Kubeconfig
		overrides := &clientcmd.ConfigOverrides{CurrentContext: cfg.Context}
		return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	}
	if rc, err := rest.InClusterConfig(); err == nil {
		return rc, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}
