package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
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

// With WithToolElicitor set, a question an MCP server asks mid-call
// reaches the product's elicitor: the kit dials the client with
// mcpclient.WithElicitation, without which the client offers none and
// the server cannot ask.
func TestAnMCPServersQuestionReachesTheKitsElicitor(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "deploy", Description: "deploy after asking"},
		// From protocol 2026-07-28 a server asks by returning the question
		// as the call's input request, and the answer comes with the
		// client's retry of the call.
		func(_ context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			answer, ok := req.Params.InputResponses["confirm"].(*sdk.ElicitResult)
			if !ok {
				return &sdk.CallToolResult{InputRequests: sdk.InputRequestMap{"confirm": &sdk.ElicitParams{
					Message:         "Deploy to production?",
					RequestedSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
				}}}, nil, nil
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: answer.Action}}}, nil, nil
		})
	client, server := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(t.Context(), server, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	var asked atomic.Int32
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMCPTransport(client),
		agentkit.WithToolElicitor(agentpolicy.ByHuman, func(_ context.Context, e agenttool.Elicitation) (agenttool.Answer, error) {
			asked.Add(1)
			return agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{"ok":true}`)}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	deploy, ok := kit.LookupTool("deploy")
	if !ok {
		t.Fatal("the server's tool is not in the union")
	}
	ctx := agenttool.ContextWithElicitor(t.Context(), kit.Config().ToolElicitor)
	res, err := deploy.Execute(ctx, agenttool.Call{ID: "call-1", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if asked.Load() != 1 {
		t.Fatalf("the elicitor was asked %d times, want once", asked.Load())
	}
	if !strings.Contains(res.Output.String(), "accept") {
		t.Fatalf("the tool's output = %q, want the server to have read the answer", res.Output)
	}
}

// A server added after New is offered from the next resolution on, after
// the servers New dialed and ahead of the product's provider, labelled
// as WithMCPTransport's would be, and a call reaches it. (#50)
func TestAServerAddedAfterNewJoinsTheTools(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithMCPTransport(serveMCP(t, "remote_one")),
		agentkit.WithToolProvider(func(context.Context) []agenttool.Tool {
			return []agenttool.Tool{namedTool(t, "provided")}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	label, err := kit.AddMCPTransport(t.Context(), serveMCP(t, "remote_two"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(label, "mcp:#2 ") {
		t.Fatalf("label = %q, want it numbered after New's server", label)
	}
	tools := kit.Config().ResolveTools(t.Context())
	if got := strings.Join(toolNames(tools), " "); got != "read remote_one remote_two provided" {
		t.Fatalf("tools = %s, want the added server after New's and before the provider's", got)
	}
	var origin string
	for _, o := range kit.Tools() {
		if o.Name == "remote_two" {
			origin = o.Source
		}
	}
	if origin != label {
		t.Fatalf("Kit.Tools names remote_two's source %q, want %q", origin, label)
	}
	for _, tool := range tools {
		if tool.Name() == "remote_two" {
			if _, err := tool.Execute(t.Context(), agenttool.Call{ID: "c1", Args: json.RawMessage(`{}`)}); err != nil {
				t.Fatalf("a call to the added server = %v", err)
			}
		}
	}
}

// A server whose tool takes a name the kit offers now is refused and
// closed, whether the name is before it or a provider's after it: this
// is when the product can still prefix it. (#50)
func TestAServerAddedWithATakenNameIsRefused(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithToolProvider(func(context.Context) []agenttool.Tool {
			return []agenttool.Tool{namedTool(t, "provided")}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	for _, name := range []string{"read", "provided"} {
		if _, err := kit.AddMCPTransport(t.Context(), serveMCP(t, name)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("adding a server offering %s = %v, want a conflict naming it", name, err)
		}
	}
	if got := strings.Join(toolNames(kit.Config().ResolveTools(t.Context())), " "); got != "read provided" {
		t.Fatalf("tools = %s after two refused servers, want them unchanged", got)
	}
	if _, err := kit.AddMCPTransport(t.Context(), serveMCP(t, "read"), mcpclient.WithPrefix("fs")); err != nil {
		t.Fatalf("a prefixed server = %v, want it added", err)
	}
}

// RemoveMCP drops an added server's tools and closes it, and refuses a
// label that names no added server; Close closes what is left, and a
// server added after it is refused. (#50)
func TestAnAddedServerIsRemovedAndClosed(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMCPTransport(serveMCP(t, "remote_one")),
	)
	if err != nil {
		t.Fatal(err)
	}
	two, err := kit.AddMCPTransport(t.Context(), serveMCP(t, "remote_two"))
	if err != nil {
		t.Fatal(err)
	}
	three, err := kit.AddMCPTransport(t.Context(), serveMCP(t, "remote_three"))
	if err != nil {
		t.Fatal(err)
	}
	if err := kit.RemoveMCP(two); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toolNames(kit.Config().ResolveTools(t.Context())), " "); got != "remote_one remote_three" {
		t.Fatalf("tools = %s after removing %s", got, two)
	}
	for _, o := range kit.Tools() {
		if o.Source == two {
			t.Fatalf("Kit.Tools still lists %s from a removed server", o.Name)
		}
	}
	if err := kit.RemoveMCP(two); err == nil {
		t.Error("removing a server twice was accepted")
	}
	if err := kit.RemoveMCP(kit.Tools()[0].Source); err == nil {
		t.Error("removing a server New dialed was accepted")
	}

	if err := kit.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(kit.Config().ResolveTools(t.Context())); n != 0 {
		t.Fatalf("tools = %d after Close, want 0", n)
	}
	if err := kit.RemoveMCP(three); err == nil {
		t.Error("Close left the added server to be removed")
	}
	if _, err := kit.AddMCPTransport(t.Context(), serveMCP(t, "remote_four")); err == nil {
		t.Error("a server added after Close was accepted")
	}
}

// A kit with no tool source has no ToolProvider in its config, so there
// is nothing to offer an added server's tools through. (#50)
func TestAddingAServerToAKitWithNoToolsIsAnError(t *testing.T) {
	kit, err := agentkit.New(t.Context(), agentkit.WithModel(stubModel{}, "m"))
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	if _, err := kit.AddMCPTransport(t.Context(), serveMCP(t, "remote_one")); err == nil {
		t.Fatal("a server was added to a kit whose config has no ToolProvider")
	}
}

// In a session under way, the request after AddMCP offers the server's
// tools and the session records them arriving. (#50)
func TestAServerAddedMidSessionIsRecorded(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	model := &scriptModel{}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := kit.AddMCPTransport(t.Context(), serveMCP(t, "remote_one")); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("two")); err != nil {
		t.Fatal(err)
	}
	offered := func(req openresponses.Request) bool {
		for _, tool := range req.Tools {
			if strings.Contains(string(mustJSON(t, tool)), `"remote_one"`) {
				return true
			}
		}
		return false
	}
	reqs := model.requests()
	if len(reqs) != 2 || offered(reqs[0]) || !offered(reqs[1]) {
		t.Fatalf("remote_one offered in %d requests: want only the one after AddMCP", len(reqs))
	}
	var added bool
	for _, c := range configEntries(openSession(t, sessions, kit.SessionID())) {
		for _, tool := range c.ToolsAdded {
			added = added || strings.Contains(string(mustJSON(t, tool)), `"remote_one"`)
		}
	}
	if !added {
		t.Fatal("the session does not record remote_one arriving")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
