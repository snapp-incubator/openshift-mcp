// Package mcp implements the OpenShift/Kubernetes observability MCP server:
// read-only tools over the Kubernetes API (pods, workloads, services, events,
// logs, routes, quotas, nodes) that give an AI agent cluster vision to diagnose
// issues and recommend fixes. Nothing here mutates cluster state.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
	"github.com/snapp-incubator/openshift-mcp/internal/version"
)

// args is a decoded tool-call argument map with typed getters.
type args map[string]any

func (a args) str(key string) string {
	v, _ := a[key].(string)
	return v
}

func (a args) boolean(key string) bool {
	v, _ := a[key].(bool)
	return v
}

func (a args) intOr(key string, def int) int {
	switch v := a[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

// handlerFunc is a tool handler. It returns any JSON-marshalable value (or a
// plain string, passed through as-is). Errors become MCP error results.
type handlerFunc func(ctx context.Context, c *k8s.Client, a args) (any, error)

// tool couples a definition with its handler.
type tool struct {
	name        string
	description string
	schema      map[string]any
	handler     handlerFunc
}

// Server is the MCP server over a Kubernetes client.
type Server struct {
	mcpServer *sdkmcp.Server
	client    *k8s.Client
	log       *slog.Logger
}

// NewServer builds the server and registers all tools.
func NewServer(client *k8s.Client, log *slog.Logger) *Server {
	s := &Server{client: client, log: log}
	s.mcpServer = sdkmcp.NewServer(
		&sdkmcp.Implementation{Name: "openshift-mcp", Version: version.String()},
		&sdkmcp.ServerOptions{Instructions: instructions},
	)
	for _, t := range buildTools() {
		s.addTool(t)
	}
	return s
}

// addTool registers one tool, wrapping its handler with argument decoding,
// logging, and JSON rendering of results.
func (s *Server) addTool(t tool) {
	handler := t.handler
	s.mcpServer.AddTool(
		&sdkmcp.Tool{Name: t.name, Description: t.description, InputSchema: t.schema},
		func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			var a args
			if len(req.Params.Arguments) > 0 {
				_ = json.Unmarshal(req.Params.Arguments, &a)
			}

			start := time.Now()
			s.log.Info("tool call", "tool", t.name, "args", map[string]any(a))

			out, err := handler(ctx, s.client, a)
			duration := time.Since(start)
			if err != nil {
				s.log.Warn("tool error", "tool", t.name, "duration", duration, "err", err)
				return errResult("%v", err), nil
			}

			text, ok := out.(string)
			if !ok {
				b, merr := json.MarshalIndent(out, "", "  ")
				if merr != nil {
					return errResult("marshal result: %v", merr), nil
				}
				text = string(b)
			}
			s.log.Info("tool result", "tool", t.name, "duration", duration, "bytes", len(text))
			return textResult(text), nil
		},
	)
}

// RunStdio serves MCP over stdin/stdout.
func (s *Server) RunStdio(ctx context.Context) error {
	return s.mcpServer.Run(ctx, &sdkmcp.StdioTransport{})
}

// HTTPHandler serves MCP over streamable HTTP. Stateless: these tools are pure
// request/response, so no per-client session state accumulates.
func (s *Server) HTTPHandler() http.Handler {
	return sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return s.mcpServer },
		&sdkmcp.StreamableHTTPOptions{Stateless: true},
	)
}

func textResult(text string) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: text}}}
}

func errResult(format string, a ...any) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: fmt.Sprintf(format, a...)}},
		IsError: true,
	}
}

const instructions = `You are a Kubernetes/OpenShift observability assistant. All tools are strictly
read-only: you inspect cluster state, explain what is wrong, and recommend fixes
for the user to apply — you never change anything.

Investigation workflows:
1. Unhealthy app: list_pods (check phase/ready/restarts/reason) -> get_pod (container
   states, conditions, recent events) -> pod_logs (previous=true after crashes).
2. Rollout stuck: get_workload (conditions, ready vs desired) -> list_events
   (warnings) -> get_quota (quota exhaustion blocks new pods).
3. Service unreachable: get_service (selector + endpoint readiness; 0 ready
   endpoints = selector mismatch or pods not ready) -> list_pods with the selector.
4. Route/ingress (OpenShift): list_routes (host, target service, admitted status).
5. Pending pods: get_pod events (FailedScheduling reason), list_nodes/get_node
   (pressure conditions, allocatable), get_quota.
6. Resource hunger: top_pods (live CPU/memory) vs container requests/limits in get_pod.
7. Anything else (CRDs, OpenShift objects): get_resource / list_resource with the
   exact group/version/resource plural.

Tips:
- Always pass namespace where the tool accepts it.
- pod_logs previous=true shows logs from before the last crash — essential for
  CrashLoopBackOff.
- Events expire quickly (~1h); absence of events does not mean absence of problems.
- Summaries omit unchanged/healthy detail; drill into a single object for depth.`
