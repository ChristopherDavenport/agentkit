package agentkit_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Kit.Tools lists an MCP server's tools as the model is offered them,
// the server's list now, not the one it gave when it was added: a
// server that adds and removes tools, and tells the client the list
// changed, is listed with its new set, under its label and through
// WithToolFilter. (#101)
func TestKitToolsFollowsAnMCPServersList(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "changing", Version: "0"}, nil)
	add := func(name string) {
		sdk.AddTool(srv, &sdk.Tool{Name: name, Description: name + " does nothing"},
			func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
				return &sdk.CallToolResult{}, nil, nil
			})
	}
	add("old")
	add("hidden")
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithToolFilter(func(_ string, tool agenttool.Tool) bool { return !strings.HasPrefix(tool.Name(), "hidden") }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	label, err := kit.AddMCPTransport(t.Context(), serveServer(t, srv))
	if err != nil {
		t.Fatal(err)
	}

	add("new")
	add("hidden_too")
	srv.RemoveTools("old")
	// The client refreshes its list when the server says it changed;
	// wait until the model is offered the new set.
	deadline := time.Now().Add(5 * time.Second)
	for {
		offered := toolNames(kit.Config().ResolveTools(t.Context()))
		if slices.Contains(offered, "new") && !slices.Contains(offered, "old") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the model is offered %v; the client never took the server's new list", offered)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var got []string
	for _, o := range kit.Tools() {
		got = append(got, o.Name+"@"+o.Source)
	}
	want := []string{"read@WithTools", "new@" + label}
	if !slices.Equal(got, want) {
		t.Fatalf("Kit.Tools = %v, want %v", got, want)
	}
}
