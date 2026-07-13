// Command openshift-mcp is a read-only OpenShift/Kubernetes observability MCP
// server: pods, workloads, services, routes, events, logs, quotas, and nodes,
// summarized for AI agents. It never mutates cluster state.
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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gitlab.snapp.ir/snappcloud/openshift-mcp/internal/k8s"
	"gitlab.snapp.ir/snappcloud/openshift-mcp/internal/mcp"
	"gitlab.snapp.ir/snappcloud/openshift-mcp/internal/version"
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
	runHTTP(ctx, srv, client, log, *httpAddr)
}

func runHTTP(ctx context.Context, srv *mcp.Server, client *k8s.Client, log *slog.Logger, addr string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		checkCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		// Cheap API-server reachability probe.
		if _, err := client.Clientset.CoreV1().Namespaces().List(checkCtx, metav1.ListOptions{Limit: 1}); err != nil {
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
