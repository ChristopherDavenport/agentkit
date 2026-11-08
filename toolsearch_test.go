package agentkit_test

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// describedTool is a tool with a description of its own, which counts
// its calls.
func describedTool(name, description string, calls *atomic.Int32) agenttool.Tool {
	return agenttool.New(name, description, func(context.Context, agenttool.NoArgs) (string, error) {
		if calls != nil {
			calls.Add(1)
		}
		return name + " ran", nil
	})
}

// deferNamed holds back the tools of the given names, whatever their
// source.
func deferNamed(names ...string) func(string, agenttool.Tool) bool {
	return func(_ string, t agenttool.Tool) bool { return slices.Contains(names, t.Name()) }
}

// searchTools is the set the tests below hold back two of.
func searchTools(weatherCalls *atomic.Int32) []agenttool.Tool {
	return []agenttool.Tool{
		describedTool("read", "Read a file.", nil),
		describedTool("weather_lookup", "Look up the weather for a city. Returns the temperature and the sky, in metric units, for today and the next six days, from the national service.", weatherCalls),
		describedTool("stock_quote", "Quote a stock's last price.\n\nIt says nothing of the weather.", nil),
	}
}

func TestTheListingNamesEachHeldToolWithItsFirstSentence(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithMemory(memStore(t), "user"),
		agentkit.WithTools(searchTools(nil)...),
		agentkit.WithToolSearch(deferNamed("weather_lookup", "stock_quote")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	var listing string
	for _, p := range kit.Parts() {
		if p.ID == agentkit.PartToolSearch {
			listing = p.Text
			if p.Source != agentkit.SourceToolSearch {
				t.Errorf("source = %q, want %q", p.Source, agentkit.SourceToolSearch)
			}
		}
	}
	for _, want := range []string{
		"## Tools loaded on request",
		agentkit.ToolSearchTool,
		"\n- weather_lookup: Look up the weather for a city.\n",
		"\n- stock_quote: Quote a stock's last price.",
	} {
		if !strings.Contains(listing+"\n", want) {
			t.Errorf("listing lacks %q:\n%s", want, listing)
		}
	}
	if strings.Contains(listing, "read") && strings.Contains(listing, "- read") {
		t.Errorf("listing names a tool that is not held:\n%s", listing)
	}
	if !strings.Contains(kit.Config().Instructions, listing) {
		t.Error("the configuration's instructions lack the listing New rendered")
	}
	// After the product's prompt, before the memory block.
	ids := groupIDs(kit.Parts())
	at, mem := slices.Index(ids, agentkit.PartToolSearch), slices.Index(ids, agentkit.PartMemory)
	if at < 0 || mem < 0 || at > mem || ids[0] != agentkit.PartProduct {
		t.Errorf("parts = %v, want the listing after the product's prompt and before memory", ids)
	}

	names := toolNames(kit.Config().ResolveTools(t.Context()))
	if slices.Contains(names, "weather_lookup") || slices.Contains(names, "stock_quote") || names[len(names)-1] != agentkit.ToolSearchTool {
		t.Errorf("tools = %v, want the held tools out and tool_search last", names)
	}
}

func TestASummaryIsOneSentenceAndCapped(t *testing.T) {
	long := strings.Repeat("word ", 60)
	cases := []struct{ in, want string }{
		{"Search the web.  Use it\nfor news.", "Search the web."},
		{"no period at all", "no period at all"},
		{"", ""},
	}
	for _, c := range cases {
		if got := agentkit.ToolSummary(c.in); got != c.want {
			t.Errorf("ToolSummary(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	got := agentkit.ToolSummary(long)
	if n := len([]rune(got)); n != agentkit.ToolSummaryRunes || !strings.HasSuffix(got, "…") {
		t.Errorf("a long description's summary is %d runes, %q; want %d ending in an ellipsis", n, got, agentkit.ToolSummaryRunes)
	}
}

func TestNothingHeldSendsNeitherTheListingNorTheTool(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(searchTools(nil)...),
		agentkit.WithToolSearch(deferNamed()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	if ids := groupIDs(kit.Parts()); slices.Contains(ids, agentkit.PartToolSearch) {
		t.Errorf("parts = %v, want no listing when nothing is held", ids)
	}
	if names := toolNames(kit.Config().ResolveTools(t.Context())); slices.Contains(names, agentkit.ToolSearchTool) {
		t.Errorf("tools = %v, want no tool_search when nothing is held", names)
	}
}

// A load by exact name and one by keyword each put the tool in the next
// request, after the tools already sent: each request's tools are the
// last one's with the loaded appended, so the cached prefix holds.
func TestLoadedToolsAreAppendedFromTheNextRequestOn(t *testing.T) {
	var weather atomic.Int32
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentkit.ToolSearchTool, `{"query":"select:weather_lookup"}`),
		callTurn("weather_lookup", `{}`),
		callTurn(agentkit.ToolSearchTool, `{"query":"stock price"}`),
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithTools(searchTools(&weather)...),
		agentkit.WithToolSearch(deferNamed("weather_lookup", "stock_quote")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	end, err := agent.Prompt(t.Context(), openresponses.UserText("weather and stocks"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonDone {
		t.Fatalf("reason = %q, want done", end.Reason)
	}
	if weather.Load() != 1 {
		t.Fatalf("weather_lookup ran %d times, want once after it was loaded", weather.Load())
	}
	reqs := model.requests()
	if len(reqs) != 4 {
		t.Fatalf("%d requests, want 4", len(reqs))
	}
	want := [][]string{
		{"read", agentkit.ToolSearchTool},
		{"read", agentkit.ToolSearchTool, "weather_lookup"},
		{"read", agentkit.ToolSearchTool, "weather_lookup"},
		{"read", agentkit.ToolSearchTool, "weather_lookup", "stock_quote"},
	}
	for i, req := range reqs {
		got := requestToolNames(req)
		if !slices.Equal(got, want[i]) {
			t.Errorf("request %d tools = %v, want %v", i+1, got, want[i])
		}
		if i > 0 {
			prev := reqs[i-1].Tools
			if len(req.Tools) < len(prev) {
				t.Fatalf("request %d sent fewer tools than the one before", i+1)
			}
			for j := range prev {
				if prev[j].(*openresponses.FunctionTool).Name != req.Tools[j].(*openresponses.FunctionTool).Name {
					t.Errorf("request %d moved tool %d; the prefix must hold", i+1, j)
				}
			}
		}
	}
	// The instructions are the same in every request: the listing does
	// not change as tools are loaded.
	for i, req := range reqs[1:] {
		if req.Instructions != reqs[0].Instructions {
			t.Errorf("request %d changed the instructions", i+2)
		}
	}
	if got := kit.ToolSearch().Loaded(t.Context()); !slices.Equal(got, []string{"weather_lookup", "stock_quote"}) {
		t.Errorf("Loaded = %v, want both in load order", got)
	}
}

func TestToolSearchSaysWhatItLoaded(t *testing.T) {
	held := searchTools(nil)[1:]
	s := agentkit.NewToolSearch(func(context.Context) []agenttool.Tool { return held }, nil)
	run := func(args string) string {
		t.Helper()
		res, err := s.Tool().Execute(t.Context(), agenttool.Call{ID: "c", Args: []byte(args)})
		if err != nil {
			t.Fatal(err)
		}
		return res.Output.Text
	}
	if out := run(`{"query":"select:stock_quote, nope"}`); !strings.Contains(out, "1 tool(s) loaded") || !strings.Contains(out, "stock_quote") || !strings.Contains(out, `"nope"`) {
		t.Errorf("select output = %q, want stock_quote loaded and nope named unknown", out)
	}
	if out := run(`{"query":"select:stock_quote"}`); !strings.Contains(out, "loaded already") {
		t.Errorf("a second select = %q, want it said to be loaded already", out)
	}
	// "weather" is in weather_lookup's name and only in stock_quote's
	// description: the name match first, and +weather keeps it alone.
	if out := run(`{"query":"+weather"}`); !strings.Contains(out, "weather_lookup") || strings.Contains(out, "stock_quote") {
		t.Errorf("+weather = %q, want weather_lookup alone", out)
	}
	if out := run(`{"query":"unrelated"}`); !strings.Contains(out, "No deferred tool matched") {
		t.Errorf("no match = %q", out)
	}
	if got := s.Loaded(t.Context()); !slices.Equal(got, []string{"stock_quote", "weather_lookup"}) {
		t.Errorf("Loaded = %v, want stock_quote then weather_lookup", got)
	}
	if _, err := s.Tool().Execute(t.Context(), agenttool.Call{ID: "c", Args: []byte(`{"query":" "}`)}); err == nil {
		t.Error("an empty query was not refused")
	}
}

// Each conversation loads its own, by its grant scope: a child agent
// runs under a scope of its own, so it starts with none loaded and sees
// the same listing.
func TestAnotherConversationStartsWithNoneLoaded(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(searchTools(nil)...),
		agentkit.WithToolSearch(deferNamed("weather_lookup", "stock_quote")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	ts := kit.ToolSearch()
	ts.Load(t.Context(), "weather_lookup")
	child := agentpolicy.ContextWithGrantScope(t.Context(), kit.GrantScope(t.Context())+"/child-1")
	if got := ts.Loaded(child); len(got) != 0 {
		t.Errorf("the child has %v loaded, want none", got)
	}
	if names := toolNames(kit.Config().ResolveTools(child)); !slices.Equal(names, []string{"read", agentkit.ToolSearchTool}) {
		t.Errorf("the child is offered %v, want none of the parent's loads", names)
	}
	if names := toolNames(kit.Config().ResolveTools(t.Context())); !slices.Equal(names, []string{"read", agentkit.ToolSearchTool, "weather_lookup"}) {
		t.Errorf("the parent is offered %v, want its load", names)
	}
}

// The policy is the same for a held tool: loading it is not running it,
// and the call is decided as any other's.
func TestTheEngineDecidesALoadedToolsCall(t *testing.T) {
	var weather atomic.Int32
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentkit.ToolSearchTool, `{"query":"select:weather_lookup"}`),
		callTurn("weather_lookup", `{}`),
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithTools(searchTools(&weather)...),
		agentkit.WithToolSearch(deferNamed("weather_lookup", "stock_quote")),
		agentkit.WithPolicy(agentpolicy.Policy{
			Default: agentpolicy.Ask(),
			Allow:   []agentpolicy.Rule{{Tool: agentkit.ToolSearchTool}},
			Deny:    []agentpolicy.Rule{{Tool: "stock_quote"}},
		}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	// A bare deny takes the tool off the listing as off the list.
	for _, p := range kit.Parts() {
		if p.ID == agentkit.PartToolSearch && strings.Contains(p.Text, "stock_quote") {
			t.Errorf("the listing names stock_quote, which a bare deny removes:\n%s", p.Text)
		}
	}

	agent := agentturn.New(kit.Config())
	end, err := agent.Prompt(t.Context(), openresponses.UserText("weather"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 1 || end.Pending[0].Call.Name != "weather_lookup" {
		t.Fatalf("end = %q with %d pending, want weather_lookup's call held", end.Reason, len(end.Pending))
	}
	if weather.Load() != 0 {
		t.Fatal("the held call ran")
	}
}

// The record is the config entry's tools_added, and a resumed
// conversation starts with the tools its record has in force: its first
// request offers what its last one did, in the same order.
func TestAResumedConversationKeepsItsLoadedTools(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentkit.ToolSearchTool, `{"query":"select:stock_quote,weather_lookup"}`),
	}}
	opts := func(extra ...agentkit.Option) []agentkit.Option {
		return append([]agentkit.Option{
			agentkit.WithModel(model, "m"),
			agentkit.WithTools(searchTools(nil)...),
			agentkit.WithToolSearch(deferNamed("weather_lookup", "stock_quote")),
		}, extra...)
	}
	first, err := agentkit.New(t.Context(), opts(agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))...)
	if err != nil {
		t.Fatal(err)
	}
	agent := agentturn.New(first.Config())
	unsubscribe := first.Attach(agent)
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("load them")); err != nil {
		t.Fatal(err)
	}
	unsubscribe()
	id := first.SessionID()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	lastFirst := requestToolNames(model.requests()[len(model.requests())-1])

	sess, err := sessions.Open(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var added []string
	for _, e := range sess.Path(sess.Leaf()) {
		if c, ok := e.(*agentsession.ConfigEntry); ok && !c.Replace {
			for _, tool := range c.ToolsAdded {
				added = append(added, agentsession.ToolName(tool))
			}
		}
	}
	if !slices.Equal(added, []string{"stock_quote", "weather_lookup"}) {
		t.Errorf("tools_added on the path = %v, want the two loaded, in load order", added)
	}

	second, err := agentkit.New(t.Context(), opts(agentkit.WithResumedSession(sessions, id))...)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if got := second.ToolSearch().Loaded(t.Context()); !slices.Equal(got, []string{"stock_quote", "weather_lookup"}) {
		t.Errorf("the resumed conversation has %v loaded, want both, in load order", got)
	}
	resumed := agentturn.New(second.Config(), second.AgentOptions()...)
	defer second.Attach(resumed)()
	n := len(model.requests())
	if _, err := resumed.Prompt(t.Context(), openresponses.UserText("again")); err != nil {
		t.Fatal(err)
	}
	if got := requestToolNames(model.requests()[n]); !slices.Equal(got, lastFirst) {
		t.Errorf("the resumed first request offers %v, want the last request's %v", got, lastFirst)
	}
}

func TestToolsLabelsTheHeldTools(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(searchTools(nil)...),
		agentkit.WithToolSearch(deferNamed("stock_quote")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	got := kit.Tools()
	want := []agentkit.ToolOrigin{
		{Name: "read", Source: "WithTools"},
		{Name: "weather_lookup", Source: "WithTools"},
		{Name: "stock_quote", Source: "WithTools", Deferred: true},
		{Name: agentkit.ToolSearchTool, Source: "WithToolSearch"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Tools = %v, want %v", got, want)
	}
}

func TestDeferMCPToolsHoldsAServersTools(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithMCPTransport(serveMCP(t, "remote_one", "remote_two")),
		agentkit.WithToolSearch(agentkit.DeferMCPTools),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	if names := toolNames(kit.Config().ResolveTools(t.Context())); !slices.Equal(names, []string{"read", agentkit.ToolSearchTool}) {
		t.Errorf("tools = %v, want the server's held", names)
	}
	var listing string
	for _, p := range kit.Parts() {
		if p.ID == agentkit.PartToolSearch {
			listing = p.Text
		}
	}
	if !strings.Contains(listing, "- remote_one: remote_one does nothing") || !strings.Contains(listing, "- remote_two") {
		t.Errorf("listing = %q, want both of the server's tools, listed at New", listing)
	}
}

func TestAToolNamedToolSearchIsAConflict(t *testing.T) {
	_, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, agentkit.ToolSearchTool), namedTool(t, "x")),
		agentkit.WithToolSearch(deferNamed("x")),
	)
	if err == nil || !strings.Contains(err.Error(), "WithToolSearch") {
		t.Fatalf("err = %v, want a conflict naming WithToolSearch", err)
	}
}
