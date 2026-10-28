// Command openshift-mcp is a read-only OpenShift/Kubernetes observability MCP
// server: pods, workloads, services, routes, events, logs, quotas, and nodes,
// summarized for AI agents. Its /mcp tools never mutate cluster state; the
// optional /debug/mcp endpoint starts debug pods as the caller.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/snapp-incubator/openshift-mcp/internal/debugpod"
	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
	"github.com/snapp-incubator/openshift-mcp/internal/mcp"
	"github.com/snapp-incubator/openshift-mcp/internal/version"
)

func main() {
	mcpMode := flag.Bool("mcp", false, "run as stdio MCP server (for local MCP clients)")
	httpAddr := flag.String("http-addr", ":8080", "HTTP listen address (when not in MCP mode)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	client, err := k8s.New(k8s.ConfigFromEnv())
	if err != nil {
		log.Error("build kubernetes client", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv := mcp.NewServer(client, log)

	if *mcpMode {
		if err := srv.RunStdio(ctx); err != nil && ctx.Err() == nil {
			log.Error("stdio server", "err", err)
			os.Exit(1)
		}
		return
	}
	dbg, err := debugServer(client, log)
	if err != nil {
		log.Error("debug pods", "err", err)
		os.Exit(1)
	}
	runHTTP(ctx, srv, dbg, client, log, *httpAddr)
}

// debugServer builds the debug pod endpoint when DEBUG_POD_ENABLED=true.
func debugServer(client *k8s.Client, log *slog.Logger) (*mcp.DebugServer, error) {
	cfg, err := debugpod.ConfigFromEnv()
	if err != nil || !cfg.Enabled {
		return nil, err
	}
	token := os.Getenv("DEBUG_POD_TOKEN")
	if len(token) < 32 {
		return nil, fmt.Errorf("DEBUG_POD_TOKEN must be at least 32 characters when debug pods are enabled")
	}
	log.Info("debug pods enabled", "image", cfg.Image, "lifetime", cfg.Lifetime, "endpoint", "/debug/mcp")
	return mcp.NewDebugServer(debugpod.New(cfg, client.Rest, client.Clientset), token, log), nil
}

func runHTTP(ctx context.Context, srv *mcp.Server, dbg *mcp.DebugServer, client *k8s.Client, log *slog.Logger, addr string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// readinessTimeout bounds the API-server reachability probe. Kept generous
	// (and configurable) so a transiently slow API or a client-go rate-limiter
	// wait does not flap the pod out of Service endpoints.
	readinessTimeout := 10 * time.Second
	if v := os.Getenv("READINESS_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			readinessTimeout = d
		}
	}
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		checkCtx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		// Lightweight API-server reachability probe: a single GET /version,
		// no object listing and no RBAC — just "is the API answering".
		if _, err := client.Clientset.Discovery().RESTClient().Get().AbsPath("/version").DoRaw(checkCtx); err != nil {
			log.Warn("readiness check failed", "err", err)
			http.Error(w, "kubernetes api unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": version.String()})
	})
	mux.Handle("/mcp", srv.HTTPHandler())
	if dbg != nil {
		mux.Handle("/debug/mcp", dbg.HTTPHandler())
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	hs := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := hs.Shutdown(shutdownCtx); err != nil {
			log.Error("graceful shutdown failed", "err", err)
			_ = hs.Close()
		}
	}()

	log.Info("HTTP server listening", "addr", addr, "endpoint", "http://"+addr+"/mcp")
	if err := hs.ListenAndServe(); err != nil && ctx.Err() == nil {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "shutting down")
}
