package agentkit_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveMCP runs an MCP server offering the named tools over an
// in-memory transport, and returns the client side of it.
func serveMCP(t *testing.T, names ...string) sdk.Transport {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	for _, name := range names {
		sdk.AddTool(srv, &sdk.Tool{Name: name, Description: name + " does nothing"},
			func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
				return &sdk.CallToolResult{}, nil, nil
			})
	}
	client, server := sdk.NewInMemoryTransports()
	session, err := srv.Connect(t.Context(), server, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return client
}

func TestMCPToolsJoinTheSetAfterTheOthers(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithMemory(memStore(t), "user"),
		agentkit.WithMCPTransport(serveMCP(t, "remote_one", "remote_two")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	names := toolNames(kit.Config().ResolveTools(t.Context()))
	if names[0] != "read" {
		t.Fatalf("tools = %v, want the product's first", names)
	}
	var first int
	for i, n := range names {
		if strings.HasPrefix(n, "remote_") {
			first = i
			break
		}
	}
	if first == 0 {
		t.Fatalf("tools = %v, want the remote's after the memory tools", names)
	}
	for _, n := range names[first:] {
		if !strings.HasPrefix(n, "remote_") {
			t.Fatalf("tools = %v, want every remote tool last and together", names)
		}
	}
}

func TestAToolNameAnMCPServerRepeatsIsAnErrorAtNew(t *testing.T) {
	_, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "remote_one")),
		agentkit.WithMCPTransport(serveMCP(t, "remote_one")),
	)
	if err == nil {
		t.Fatal("two tools named remote_one were accepted")
	}
	if !strings.Contains(err.Error(), "remote_one") {
		t.Fatalf("err = %v, want it to name the tool", err)
	}
}

func TestAPrefixKeepsAnMCPServerOutOfTheWayOfTheProduct(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithMCPTransport(serveMCP(t, "read"), mcpclient.WithPrefix("fs")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	names := toolNames(kit.Config().ResolveTools(t.Context()))
	if len(names) != 2 {
		t.Fatalf("tools = %v, want both", names)
	}
}

func TestCloseClosesTheRemote(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMCPTransport(serveMCP(t, "remote_one")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(kit.Config().ResolveTools(t.Context())); n != 1 {
		t.Fatalf("tools = %d, want 1", n)
	}
	if err := kit.Close(); err != nil {
		t.Fatal(err)
	}
	// The remote is gone, so the provider offers nothing and the loop
	// sees an empty set rather than a tool it cannot reach.
	if n := len(kit.Config().ResolveTools(t.Context())); n != 0 {
		t.Fatalf("tools = %d after Close, want 0", n)
	}
}

func TestAnEmptyMCPCommandIsAnError(t *testing.T) {
	_, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMCP("   "),
	)
	if err == nil || !strings.Contains(err.Error(), "empty command") {
		t.Fatalf("err = %v, want one naming the empty command", err)
	}
}

func TestSeveralMCPServersAllJoinTheSet(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithMCPTransport(serveMCP(t, "one_a", "one_b")),
		agentkit.WithMCPTransport(serveMCP(t, "two_a")),
		agentkit.WithMCPTransport(serveMCP(t, "three_a")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	names := toolNames(kit.Config().ResolveTools(t.Context()))
	want := []string{"read", "one_a", "one_b", "two_a", "three_a"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}

// Two servers that claim one name collide the same way any two sources
// do, and the error has to say which two servers, or an operator with
// six of them cannot tell which to prefix.
func TestTwoMCPServersClaimingOneNameIsAnErrorNamingBoth(t *testing.T) {
	_, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMCPTransport(serveMCP(t, "search"), mcpclient.WithClientInfo("docs", "0")),
		agentkit.WithMCPTransport(serveMCP(t, "search"), mcpclient.WithClientInfo("code", "0")),
	)
	if err == nil {
		t.Fatal("two servers named search were accepted")
	}
	var c agentkit.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("err = %v, want a Conflict", err)
	}
	if c.Kept == c.Dropped {
		t.Fatalf("conflict = %+v; both servers have the same label, so the error does not say which two", c)
	}
}

func TestPrefixesKeepSeveralServersApart(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMCPTransport(serveMCP(t, "search"), mcpclient.WithPrefix("docs")),
		agentkit.WithMCPTransport(serveMCP(t, "search"), mcpclient.WithPrefix("code")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	names := toolNames(kit.Config().ResolveTools(t.Context()))
	if len(names) != 2 {
		t.Fatalf("tools = %v, want both prefixed search tools", names)
	}
}

func TestCloseClosesEveryRemote(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMCPTransport(serveMCP(t, "one_a")),
		agentkit.WithMCPTransport(serveMCP(t, "two_a")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(kit.Config().ResolveTools(t.Context())); n != 2 {
		t.Fatalf("tools = %d, want 2", n)
	}
	if err := kit.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(kit.Config().ResolveTools(t.Context())); n != 0 {
		t.Fatalf("tools = %d after Close, want 0", n)
	}
}
