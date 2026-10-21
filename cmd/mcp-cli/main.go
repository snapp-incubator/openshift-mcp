// Command mcp-cli is a small MCP client for exercising openshift-mcp by hand:
// it connects over streamable HTTP or spawns a stdio server, then prints the
// server info, lists tools, or calls one tool with key=value arguments.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/snapp-incubator/openshift-mcp/internal/version"
)

const usage = `Usage: mcp-cli [flags] <command> [args]

Commands:
  info                          server name, version, and instructions
  tools                         list tools (-v for input schemas)
  call <tool> [key=value ...]   call a tool; values are coerced by the tool's
                                schema (integer, boolean, comma-separated array)

Examples:
  mcp-cli tools
  mcp-cli call list_pods namespace=default limit=5
  mcp-cli -cmd "go run ./cmd/openshift-mcp -mcp" call get_pod namespace=default name=web-0
  mcp-cli call list_resource -json '{"group":"batch","version":"v1","resource":"jobs","namespace":"default"}'

Flags:
`

func main() {
	fs := flag.NewFlagSet("mcp-cli", flag.ExitOnError)
	url := fs.String("url", "http://localhost:8080/mcp", "streamable HTTP endpoint (ignored when -cmd is set)")
	command := fs.String("cmd", "", `spawn a stdio server instead, e.g. "bin/openshift-mcp -mcp"`)
	timeout := fs.Duration("timeout", 60*time.Second, "overall timeout")
	verbose := fs.Bool("v", false, "tools: print input schemas")
	raw := fs.Bool("raw", false, "call: print the full CallToolResult as JSON")
	jsonArgs := fs.String("json", "", "call: arguments as a JSON object (merged under key=value args)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])

	// Allow flags after the command too: "mcp-cli call list_pods -raw ns=x".
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	cmdName, positional := rest[0], []string{}
	for args := rest[1:]; len(args) > 0; {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		args = fs.Args()
		if len(args) > 0 {
			positional = append(positional, args[0])
			args = args[1:]
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()

	session, err := connect(ctx, *url, *command)
	if err != nil {
		fatal("connect: %v", err)
	}

	switch cmdName {
	case "info":
		err = runInfo(session)
	case "tools":
		err = runTools(ctx, session, *verbose)
	case "call":
		if len(positional) == 0 {
			fatal("call: missing tool name")
		}
		err = runCall(ctx, session, positional[0], positional[1:], *jsonArgs, *raw)
	default:
		err = fmt.Errorf("unknown command (want info, tools, or call)")
	}
	_ = session.Close()
	if err != nil {
		fatal("%s: %v", cmdName, err)
	}
}

func connect(ctx context.Context, url, command string) (*sdkmcp.ClientSession, error) {
	var transport sdkmcp.Transport
	if command != "" {
		parts := strings.Fields(command)
		cmd := exec.Command(parts[0], parts[1:]...)
		// Server logs go to our stderr so stdout stays clean for results.
		cmd.Stderr = os.Stderr
		transport = &sdkmcp.CommandTransport{Command: cmd}
	} else {
		transport = &sdkmcp.StreamableClientTransport{Endpoint: url, DisableStandaloneSSE: true, MaxRetries: -1}
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "mcp-cli", Version: version.String()}, nil)
	return client.Connect(ctx, transport, nil)
}

func runInfo(session *sdkmcp.ClientSession) error {
	res := session.InitializeResult()
	if res == nil {
		return errors.New("no initialize result")
	}
	if res.ServerInfo != nil {
		fmt.Printf("server:   %s %s\n", res.ServerInfo.Name, res.ServerInfo.Version)
	}
	fmt.Printf("protocol: %s\n", res.ProtocolVersion)
	if res.Instructions != "" {
		fmt.Printf("\n%s\n", res.Instructions)
	}
	return nil
}

func runTools(ctx context.Context, session *sdkmcp.ClientSession, verbose bool) error {
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			return err
		}
		fmt.Printf("%s\n  %s\n", t.Name, t.Description)
		if verbose {
			b, _ := json.MarshalIndent(t.InputSchema, "  ", "  ")
			fmt.Printf("  %s\n", b)
		}
		fmt.Println()
	}
	return nil
}

func runCall(ctx context.Context, session *sdkmcp.ClientSession, name string, kv []string, jsonArgs string, raw bool) error {
	arguments := map[string]any{}
	if jsonArgs != "" {
		if err := json.Unmarshal([]byte(jsonArgs), &arguments); err != nil {
			return fmt.Errorf("-json: %w", err)
		}
	}
	if len(kv) > 0 {
		props, err := toolProperties(ctx, session, name)
		if err != nil {
			return err
		}
		for _, pair := range kv {
			key, value, ok := strings.Cut(pair, "=")
			if !ok {
				return fmt.Errorf("argument %q is not key=value", pair)
			}
			v, err := coerce(value, props[key])
			if err != nil {
				return fmt.Errorf("argument %q: %w", key, err)
			}
			arguments[key] = v
		}
	}

	start := time.Now()
	res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "(%s)\n", time.Since(start).Round(time.Millisecond))

	if raw {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	} else {
		for _, c := range res.Content {
			if tc, ok := c.(*sdkmcp.TextContent); ok {
				fmt.Println(tc.Text)
			} else {
				b, _ := json.Marshal(c)
				fmt.Println(string(b))
			}
		}
	}
	if res.IsError {
		return errors.New("tool returned an error result")
	}
	return nil
}

// toolProperties returns the input-schema properties of the named tool, so
// key=value arguments can be typed correctly.
func toolProperties(ctx context.Context, session *sdkmcp.ClientSession, name string) (map[string]any, error) {
	var names []string
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		if t.Name == name {
			schema, _ := t.InputSchema.(map[string]any)
			props, _ := schema["properties"].(map[string]any)
			return props, nil
		}
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return nil, fmt.Errorf("unknown tool %q (available: %s)", name, strings.Join(names, ", "))
}

// coerce converts a command-line string to the JSON type the schema expects.
// Properties without a known type are parsed as JSON when possible, else kept
// as strings.
func coerce(value string, prop any) (any, error) {
	p, _ := prop.(map[string]any)
	switch p["type"] {
	case "string":
		return value, nil
	case "integer":
		return strconv.Atoi(value)
	case "number":
		return strconv.ParseFloat(value, 64)
	case "boolean":
		return strconv.ParseBool(value)
	case "array":
		if strings.HasPrefix(value, "[") {
			var out []any
			err := json.Unmarshal([]byte(value), &out)
			return out, err
		}
		out := []any{}
		for _, s := range strings.Split(value, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	}
	var v any
	if err := json.Unmarshal([]byte(value), &v); err == nil {
		return v, nil
	}
	return value, nil
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "mcp-cli: "+format+"\n", a...)
	os.Exit(1)
}
