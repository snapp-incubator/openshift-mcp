package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/snapp-incubator/openshift-mcp/internal/debugpod"
	"github.com/snapp-incubator/openshift-mcp/internal/version"
)

// debugRunner is what the debug tools drive; *debugpod.Runner in production.
type debugRunner interface {
	Start(ctx context.Context, user, ns, reason string) (*debugpod.Started, error)
	Exec(ctx context.Context, user, ns, pod, command string, timeout time.Duration) (string, error)
	Finish(ctx context.Context, user, ns, pod string) error
	List(ctx context.Context, user, ns string) ([]map[string]any, error)
}

// DebugServer serves the debug pod tools on their own endpoint. Every call
// acts as the user named in X-Remote-User, so the endpoint takes a bearer
// token of its own: only the bot may name a user.
type DebugServer struct {
	mcpServer *sdkmcp.Server
	token     string
	log       *slog.Logger
}

// NewDebugServer builds the debug endpoint. token must not be empty.
func NewDebugServer(r debugRunner, token string, log *slog.Logger) *DebugServer {
	s := &DebugServer{token: token, log: log}
	s.mcpServer = sdkmcp.NewServer(
		&sdkmcp.Implementation{Name: "openshift-mcp-debug", Version: version.String()},
		&sdkmcp.ServerOptions{Instructions: debugInstructions},
	)
	for _, t := range debugTools(r) {
		s.add(t)
	}
	return s
}

type debugTool struct {
	name, description string
	schema            map[string]any
	run               func(ctx context.Context, user string, a args) (any, error)
}

func (s *DebugServer) add(t debugTool) {
	s.mcpServer.AddTool(&sdkmcp.Tool{Name: t.name, Description: t.description, InputSchema: t.schema},
		func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			var a args
			if len(req.Params.Arguments) > 0 {
				_ = json.Unmarshal(req.Params.Arguments, &a)
			}
			user := ""
			if req.Extra != nil && req.Extra.Header != nil {
				user = strings.TrimSpace(req.Extra.Header.Get("X-Remote-User"))
			}
			start := time.Now()
			out, err := t.run(ctx, user, a)
			if err != nil {
				s.log.Warn("debug tool error", "tool", t.name, "user", user, "namespace", a.str("namespace"),
					"duration", time.Since(start), "err", err)
				return errResult("%v", err), nil
			}
			s.log.Info("debug tool", "tool", t.name, "user", user, "namespace", a.str("namespace"),
				"pod", a.str("pod"), "duration", time.Since(start))
			if str, ok := out.(string); ok {
				return textResult(str), nil
			}
			b, _ := json.MarshalIndent(out, "", "  ")
			return textResult(string(b)), nil
		})
}

// HTTPHandler serves the endpoint behind its bearer token.
func (s *DebugServer) HTTPHandler() http.Handler {
	inner := sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return s.mcpServer },
		&sdkmcp.StreamableHTTPOptions{Stateless: true},
	)
	want := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if s.token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func debugTools(r debugRunner) []debugTool {
	return []debugTool{
		{
			name: "debug_pod_start",
			description: "Start a short-lived debug pod in a namespace, as the user, with network tools (curl, dig, nc). " +
				"Only after the user approved it. It sees the network as that namespace's pods do.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
				"reason":    str("What the pod is for, one sentence; recorded on the pod."),
			}, "namespace", "reason"),
			run: func(ctx context.Context, user string, a args) (any, error) {
				return r.Start(ctx, user, a.str("namespace"), a.str("reason"))
			},
		},
		{
			name: "debug_pod_exec",
			description: "Run one shell command in a debug pod you started (debug_pod_start) and return its output. " +
				"Non-zero exit codes are returned with the output.",
			schema: objSchema(map[string]any{
				"namespace":       str(nsDesc),
				"pod":             str("Debug pod name from debug_pod_start."),
				"command":         str("Shell command, e.g. 'dig +short api.example.svc' or 'curl -sv -m 5 http://svc:8080/healthz'."),
				"timeout_seconds": integer("Command time limit (default and max set by the server)."),
			}, "namespace", "pod", "command"),
			run: func(ctx context.Context, user string, a args) (any, error) {
				return r.Exec(ctx, user, a.str("namespace"), a.str("pod"), a.str("command"),
					time.Duration(a.intOr("timeout_seconds", 0))*time.Second)
			},
		},
		{
			name:        "debug_pod_finish",
			description: "Finish a debug pod when the investigation is done: it exits and is deleted automatically.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
				"pod":       str("Debug pod name."),
			}, "namespace", "pod"),
			run: func(ctx context.Context, user string, a args) (any, error) {
				if err := r.Finish(ctx, user, a.str("namespace"), a.str("pod")); err != nil {
					return nil, err
				}
				return "Finished: the pod exits now and is deleted automatically.", nil
			},
		},
		{
			name:        "debug_pod_list",
			description: "List the debug pods you started in a namespace that still exist.",
			schema:      objSchema(map[string]any{"namespace": str(nsDesc)}, "namespace"),
			run: func(ctx context.Context, user string, a args) (any, error) {
				pods, err := r.List(ctx, user, a.str("namespace"))
				if err != nil {
					return nil, err
				}
				return map[string]any{"namespace": a.str("namespace"), "pods": pods}, nil
			},
		},
	}
}

const debugInstructions = `Debug pods: a short-lived pod in a tenant namespace, created as the user
(impersonated), so their own access decides. Use one only after the user approved it.
Flow: debug_pod_start -> debug_pod_exec (one command per call) -> debug_pod_finish.
Always finish the pod when done; it is also removed automatically at its deadline.
The pod has no extra capabilities: no ping or tcpdump. Use curl, dig, nc, openssl.`
