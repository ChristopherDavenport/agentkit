package agentkit_test

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
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
	changed := make(chan struct{}, 16)
	label, err := kit.AddMCPTransport(t.Context(), serveServer(t, srv), mcpclient.WithClientOptions(sdk.ClientOptions{
		ToolListChangedHandler: func(context.Context, *sdk.ToolListChangedRequest) { changed <- struct{}{} },
	}))
	if err != nil {
		t.Fatal(err)
	}
	// Each change, then the server's notification of it; Await then
	// returns once the client's list reflects every notification taken.
	for _, change := range []func(){func() { add("new") }, func() { add("hidden_too") }, func() { srv.RemoveTools("old") }} {
		change()
		select {
		case <-changed:
		case <-time.After(10 * time.Second):
			t.Fatal("the server never said its list changed")
		}
	}
	remote, ok := kit.MCP(label)
	if !ok {
		t.Fatal("no remote under the label")
	}
	if err := remote.Await(t.Context()); err != nil {
		t.Fatal(err)
	}
	if offered := toolNames(kit.Config().ResolveTools(t.Context())); !slices.Contains(offered, "new") || slices.Contains(offered, "old") {
		t.Fatalf("the model is offered %v after the client took the server's new list", offered)
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

// Kit.Tools lists what the conversation is offered: the engine's offer
// filter applies, so a tool a bare deny takes off the offer, the
// policy's or a set's under the kit's grant scope, is not listed, for
// the product's tools and a provider's alike. (#101)
func TestKitToolsLeavesOutWhatThePolicyTakesOffTheOffer(t *testing.T) {
	var ran atomic.Int32
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(counting("bash", &ran), counting("read", &ran)),
		agentkit.WithToolProvider(func(context.Context) []agenttool.Tool {
			return []agenttool.Tool{counting("fetch", &ran), counting("post", &ran)}
		}),
		agentkit.WithPolicy(agentpolicy.Policy{Deny: policyRules(t, "bash"), Default: agentpolicy.Allow()}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	ctx := agentpolicy.ContextWithGrantScope(t.Context(), kit.GrantScope(t.Context()))
	kit.Engine().GrantSet(ctx, agentpolicy.RuleSet{
		Source: agentpolicy.Source{Name: "session", Trusted: true},
		Deny:   policyRules(t, "post"),
	})
	var got []string
	for _, o := range kit.Tools() {
		got = append(got, o.Name)
	}
	if want := []string{"read", "fetch"}; !slices.Equal(got, want) {
		t.Fatalf("Kit.Tools = %v, want %v: bash and post are off the offer", got, want)
	}
}
