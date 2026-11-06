package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// pastSession writes a session created at at holding msgs into store,
// each message at at, named name.
func pastSession(t *testing.T, store agentsession.Store, name string, at time.Time, msgs ...*openresponses.Message) string {
	t.Helper()
	ctx := t.Context()
	s, err := store.Create(ctx, agentsession.Header{CreatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	info := &agentsession.InfoEntry{Name: name}
	info.Timestamp = at
	if _, err := store.Append(ctx, s.ID(), info); err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		e := agentsession.NewItemEntry(m)
		e.Timestamp = at
		if _, err := store.Append(ctx, s.ID(), e); err != nil {
			t.Fatal(err)
		}
	}
	return s.ID()
}

// functionOutput returns the text of the output the model was sent for
// call id.
func functionOutput(t *testing.T, reqs []openresponses.Request, id string) string {
	t.Helper()
	for _, req := range reqs {
		for _, it := range req.Input {
			if o, ok := it.(*openresponses.FunctionCallOutput); ok && o.CallID == id {
				return outputText(o)
			}
		}
	}
	t.Fatalf("no output for %s was sent to the model", id)
	return ""
}

// A kit-built agent answers "what did we decide about the flaky
// payments test last week" from the store's other sessions:
// conversation_search over agentsession's Search, the calling
// conversation's own session left out, a start date applied, each hit
// with its session's name and a snippet, and the output labelled
// untrusted, since earlier models and tools wrote it. (#108)
func TestConversationSearchFindsOtherSessions(t *testing.T) {
	store := agentsession.NewMemoryStore()
	day := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }
	decided := pastSession(t, store, "payments triage", day(28),
		openresponses.UserText("the flaky payments test keeps failing"),
		&openresponses.Message{Role: openresponses.RoleAssistant, Content: openresponses.Contents{
			&openresponses.OutputText{Text: "We decided to quarantine the flaky payments test until the clock fix lands."}}})
	old := pastSession(t, store, "older", day(2),
		openresponses.UserText("the flaky payments test is new"))
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurnID("call-s", agentkit.ConversationSearchTool, `{"query":"flaky payments","start_date":"2026-10-20"}`),
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithSession(store, agentsession.Header{}),
		agentkit.WithSessionSearch(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	tool, ok := kit.LookupTool(agentkit.ConversationSearchTool)
	if !ok {
		t.Fatalf("no %s tool", agentkit.ConversationSearchTool)
	}
	if flow, ok := agenttool.FlowOf(tool); !ok || !flow.ReadsLabel(agenttool.LabelUntrusted) {
		t.Errorf("flow = %+v, %v; want the output labelled untrusted", flow, ok)
	}
	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	end, err := agent.Prompt(t.Context(), openresponses.UserText("what did we decide about the flaky payments test last week?"))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("run ended %+v, %v", end, err)
	}
	out := functionOutput(t, model.requests(), "call-s")
	if !strings.Contains(out, decided) || !strings.Contains(out, "payments triage") || !strings.Contains(out, "quarantine the flaky payments test") {
		t.Errorf("output %q does not name the session that decided, with its name and a snippet", out)
	}
	if strings.Count(out, decided) != 2 {
		t.Errorf("output %q: want both of %s's messages", out, decided)
	}
	if strings.Contains(out, old) {
		t.Errorf("output %q holds a session before the start date", out)
	}
	if strings.Contains(out, kit.SessionID()) || strings.Contains(out, "last week") {
		t.Errorf("output %q holds the calling conversation's own session", out)
	}
}

// agentsession's Query bounds a message's time exclusively at both ends;
// the tool's dates are inclusive, an end_date's whole UTC day included,
// and roles keep only those messages. A bad role or an empty range is
// the model's to fix. (#108)
func TestConversationSearchDatesAreInclusive(t *testing.T) {
	store := agentsession.NewMemoryStore()
	at := time.Date(2026, 10, 28, 23, 59, 59, 0, time.UTC)
	id := pastSession(t, store, "late", at,
		openresponses.UserText("deploy window"),
		&openresponses.Message{Role: openresponses.RoleAssistant, Content: openresponses.Contents{
			&openresponses.OutputText{Text: "the deploy window is friday"}}})
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSession(store, agentsession.Header{}),
		agentkit.WithSessionSearch(agentkit.SearchLimit(1)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	tool, _ := kit.LookupTool(agentkit.ConversationSearchTool)
	run := func(args string) (string, error) {
		res, err := tool.Execute(t.Context(), agenttool.Call{ID: "c", Args: []byte(args)})
		return res.Output.Text, err
	}
	for _, c := range []struct {
		args string
		hits int
	}{
		{`{"query":"deploy","end_date":"2026-10-28","limit":5}`, 2},
		{`{"query":"deploy","start_date":"2026-10-28","end_date":"2026-10-28","limit":5}`, 2},
		{`{"query":"deploy","start_date":"2026-10-28T23:59:59Z","end_date":"2026-10-28T23:59:59Z","limit":5}`, 2},
		{`{"query":"deploy","end_date":"2026-10-28T23:59:58Z","limit":5}`, 0},
		{`{"query":"deploy","start_date":"2026-10-29","limit":5}`, 0},
		{`{"query":"deploy","roles":["user"],"limit":5}`, 1},
		{`{"query":"deploy"}`, 1},
	} {
		out, err := run(c.args)
		if err != nil {
			t.Fatalf("%s: %v", c.args, err)
		}
		if got := strings.Count(out, id); got != c.hits {
			t.Errorf("%s: %d hit(s), want %d:\n%s", c.args, got, c.hits, out)
		}
	}
	for _, args := range []string{
		`{"query":"deploy","roles":["tool"]}`,
		`{"query":"deploy","start_date":"2026-10-29","end_date":"2026-10-28"}`,
		`{"query":"deploy","start_date":"last week"}`,
	} {
		if _, err := run(args); err == nil {
			t.Errorf("%s: no error", args)
		}
	}
}

// headerSession writes a session with header h holding msgs into store,
// each message at at.
func headerSession(t *testing.T, store agentsession.Store, h agentsession.Header, at time.Time, msgs ...*openresponses.Message) string {
	t.Helper()
	s, err := store.Create(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		e := agentsession.NewItemEntry(m)
		e.Timestamp = at
		if _, err := store.Append(t.Context(), s.ID(), e); err != nil {
			t.Fatal(err)
		}
	}
	return s.ID()
}

// principalKey carries the caller a test front serves.
type principalKey struct{}

// A front serving several principals over one store, as dax's A2A peers
// share the user's, scopes each call by the caller its context names: a
// hook narrowing by that principal hides the others' sessions, and a
// hook that cannot tell who calls fails the call rather than search
// everything. The kit ships the hook; the product decides who may read
// what. (#108)
func TestConversationSearchScopeNarrowsByCaller(t *testing.T) {
	store := agentsession.NewMemoryStore()
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	user := func(name string) agentsession.Header {
		return agentsession.Header{Extra: map[string]json.RawMessage{"user": json.RawMessage(strconv.Quote(name))}}
	}
	alice := headerSession(t, store, user("alice"), at, openresponses.UserText("the release plan for alice"))
	bob := headerSession(t, store, user("bob"), at, openresponses.UserText("the release plan for bob"))
	errNoCaller := errors.New("no caller on the context")
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSession(store, agentsession.Header{}),
		agentkit.WithSessionSearch(agentkit.SearchScope(func(ctx context.Context, q *agentsession.Query) error {
			who, ok := ctx.Value(principalKey{}).(string)
			if !ok {
				return errNoCaller
			}
			q.Filter.Extra = map[string]json.RawMessage{"user": json.RawMessage(strconv.Quote(who))}
			return nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	tool, _ := kit.LookupTool(agentkit.ConversationSearchTool)
	ctx := context.WithValue(t.Context(), principalKey{}, "alice")
	res, err := tool.Execute(ctx, agenttool.Call{ID: "c", Args: []byte(`{"query":"release plan"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Output.Text; !strings.Contains(out, alice) || strings.Contains(out, bob) {
		t.Errorf("alice's search %q: want her session and not bob's", out)
	}
	if _, err := tool.Execute(t.Context(), agenttool.Call{ID: "c", Args: []byte(`{"query":"release plan"}`)}); !errors.Is(err, errNoCaller) {
		t.Errorf("a call the hook refused: err = %v, want %v", err, errNoCaller)
	}
}

// A session created after end_date holds no message before it, so the
// sessions a search reads are bounded by end_date too: the cap is spent
// on sessions that can match, and a range older than the newest
// sessions still finds its messages. (#108)
func TestConversationSearchEndDateBoundsTheSessionsRead(t *testing.T) {
	store := agentsession.NewMemoryStore()
	day := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }
	old := headerSession(t, store, agentsession.Header{CreatedAt: day(2)}, day(2), openresponses.UserText("the old migration plan"))
	headerSession(t, store, agentsession.Header{CreatedAt: day(20)}, day(20), openresponses.UserText("nothing here"))
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSession(store, agentsession.Header{}),
		agentkit.WithSessionSearch(agentkit.SearchFilter(agentsession.ListFilter{Limit: 1})),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	tool, _ := kit.LookupTool(agentkit.ConversationSearchTool)
	res, err := tool.Execute(t.Context(), agenttool.Call{ID: "c", Args: []byte(`{"query":"migration","end_date":"2026-10-05"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output.Text, old) {
		t.Errorf("output %q: want the session from before end_date, read within the cap", res.Output.Text)
	}
}
