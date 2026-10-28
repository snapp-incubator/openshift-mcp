package mcp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/snapp-incubator/openshift-mcp/internal/debugpod"
)

type stubRunner struct{ user string }

func (s *stubRunner) Start(_ context.Context, user, ns, _ string) (*debugpod.Started, error) {
	s.user = user
	return &debugpod.Started{Namespace: ns, Pod: "p"}, nil
}
func (s *stubRunner) Exec(context.Context, string, string, string, string, time.Duration) (string, error) {
	return "", nil
}
func (s *stubRunner) Finish(context.Context, string, string, string) error { return nil }
func (s *stubRunner) List(context.Context, string, string) ([]map[string]any, error) {
	return nil, nil
}

const testToken = "0123456789abcdef0123456789abcdef"

func debugCall(t *testing.T, h http.Handler, auth string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debug_pod_start","arguments":{"namespace":"team-a","reason":"x"}}}`
	req := httptest.NewRequest(http.MethodPost, "/debug/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-Remote-User", "ali@snapp.cab")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// X-Remote-User decides whose access a debug pod uses, so nobody but the
// holder of the token may set it.
func TestDebugEndpointNeedsItsToken(t *testing.T) {
	r := &stubRunner{}
	h := NewDebugServer(r, testToken, slog.New(slog.NewTextHandler(io.Discard, nil))).HTTPHandler()
	for _, auth := range []string{"", "Bearer wrong", "Basic " + testToken} {
		if rec := debugCall(t, h, auth); rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d", auth, rec.Code)
		}
	}
	if r.user != "" {
		t.Fatal("a refused request reached the runner")
	}
	rec := debugCall(t, h, "Bearer "+testToken)
	if rec.Code != http.StatusOK || r.user != "ali@snapp.cab" {
		t.Fatalf("status %d user %q body %s", rec.Code, r.user, rec.Body.String())
	}
}
