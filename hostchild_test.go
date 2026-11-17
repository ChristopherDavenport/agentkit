package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// A child the host starts itself, a sleep-time memory pass after a
// turn, is recorded as a subsession of the conversation it serves,
// linked by its spawn ID: the foreground session's export embeds the
// pass's steps and writes, DelegatedWork counts it, and after a restart
// the pass continues in the same child session. (agentsession RFC 0001
// link spawn_id, agentturn RFC 0001 The record, #109)
func TestAHostStartedPassIsALinkedSubsession(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	mem := memStore(t)
	fg, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{turns: []func(*openresponses.Emitter) error{textTurn("noted")}}, "m"),
		agentkit.WithMemory(mem, "user"),
		agentkit.WithSession(sessions, agentsession.Header{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer fg.Close()
	agent := agentturn.New(fg.Config(), fg.AgentOptions()...)
	unsubscribe := fg.Attach(agent)
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("I like tea")); err != nil {
		t.Fatal(err)
	}
	unsubscribe()

	// pass runs one memory pass under rec, started or continued.
	pass := func(rec agentkit.Option, content string, start func(*agentkit.Kit) (context.Context, error)) {
		t.Helper()
		k, err := agentkit.New(t.Context(),
			agentkit.WithModel(&scriptModel{turns: []func(*openresponses.Emitter) error{
				callTurn(agentmemory.InsertTool, `{"scope":"user","content":"`+content+`"}`),
				textTurn("done"),
			}}, "m"),
			agentkit.WithMemory(mem, "user"),
			agentkit.WithMemoryTools(agentmemory.WithInsert("user")),
			rec,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Close()
		ctx, err := start(k)
		if err != nil {
			t.Fatal(err)
		}
		child := agentturn.New(k.Config())
		defer k.AttachChild(child)()
		if _, err := child.Prompt(ctx, openresponses.UserText("consolidate memory")); err != nil {
			t.Fatal(err)
		}
	}
	spawn := agentsession.NewSpawnID()
	pass(agentkit.WithRecorder(fg.Recorder()), "likes tea", func(k *agentkit.Kit) (context.Context, error) {
		return k.StartChild(t.Context(), spawn)
	})
	if _, err := fg.StartChild(t.Context(), spawn); !errors.Is(err, agentsession.ErrSpawnIDRepeated) {
		t.Fatalf("a repeated spawn ID = %v, want ErrSpawnIDRepeated", err)
	}

	resolve := func(id string) (*agentsession.Session, error) { return sessions.Open(t.Context(), id) }
	parent := openSession(t, sessions, fg.SessionID())
	children, unread := agentsession.DelegatedWork(parent.Path(parent.Leaf()), resolve)
	childID := agentsession.SpawnedSessionID(fg.SessionID(), spawn)
	if len(children) != 1 || len(unread) != 0 || children[0].Session != childID {
		t.Fatalf("delegated work = %+v, unread %+v; want the pass's session %s", children, unread, childID)
	}
	tr, err := export.At(parent, parent.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := export.ToATIF(tr, export.Options{Subsessions: resolve})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(doc)
	if !strings.Contains(string(out), agentmemory.InsertTool) || !strings.Contains(string(out), "likes tea") {
		t.Fatalf("the foreground export does not hold the pass's write:\n%s", out)
	}

	// A restart: the conversation is resumed, and the pass continues.
	fg2, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "m"),
		agentkit.WithMemory(mem, "user"),
		agentkit.WithResumedSession(sessions, fg.SessionID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer fg2.Close()
	pass(agentkit.WithRecorder(fg2.Recorder()), "likes oolong", func(k *agentkit.Kit) (context.Context, error) {
		return k.ContinueChild(t.Context(), spawn)
	})
	child := openSession(t, sessions, childID)
	inserts := 0
	for _, e := range child.Path(child.Leaf()) {
		if it, ok := e.(*agentsession.ItemEntry); ok {
			if fc, ok := it.Item.(*openresponses.FunctionCall); ok && fc.Name == agentmemory.InsertTool {
				inserts++
			}
		}
	}
	if inserts != 2 {
		t.Fatalf("the pass's session holds %d inserts, want both passes'", inserts)
	}
}

// A pass the host starts decides under a grant scope of its own, with
// skill grants or without: the person's "allow for this session" on the
// foreground conversation does not let a sleep-time pass run the tool
// unasked. (#109 review C1)
func TestAHostStartedPassDoesNotTakeTheConversationsGrant(t *testing.T) {
	var ran atomic.Int32
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("bash", `{}`)}}, "m"),
		agentkit.WithTools(counting("bash", &ran)),
		agentkit.WithPolicy(agentpolicy.Policy{Default: agentpolicy.Ask()}, nil),
		agentkit.WithSession(agentsession.NewMemoryStore(), agentsession.Header{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	conversation := agentpolicy.ContextWithGrantScope(t.Context(), kit.GrantScope(t.Context()))
	kit.Engine().GrantSet(conversation, agentpolicy.RuleSet{
		Source: agentpolicy.Source{Name: "session", Trusted: true},
		Allow:  policyRules(t, "bash"),
	})
	ctx, err := kit.StartChild(t.Context(), agentsession.NewSpawnID())
	if err != nil {
		t.Fatal(err)
	}
	child := agentturn.New(kit.Config())
	defer kit.AttachChild(child)()
	end, err := child.Prompt(ctx, openresponses.UserText("consolidate"))
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 0 || end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("the pass ran bash %d times, ended %q; want it asked under its own scope", ran.Load(), end.Reason)
	}
}

// A pass the host starts is prompted through its own agent with the
// context StartChild returns, and its memory writes are journaled under
// the child's session. Driven through the pass kit's Control instead,
// the same writes are journaled under the session of the recorder the
// kit was built WithRecorder, the foreground's: RunContext puts that
// recorder's session on the run. The second half documents the trap
// StartChild's doc warns of; it is not a behaviour to keep. (#149)
func TestAHostStartedPassJournalsItsWritesUnderTheChildOnlyWhenPromptedDirectly(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	mem := memStore(t)
	fg, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "m"),
		agentkit.WithMemory(mem, "user"),
		agentkit.WithSession(sessions, agentsession.Header{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer fg.Close()

	// pass runs one memory pass that writes content, under a kit built
	// WithRecorder the foreground's recorder, through prompt, and returns
	// the child session StartChild named.
	pass := func(content string, prompt func(k *agentkit.Kit, ctx context.Context) error) string {
		t.Helper()
		k, err := agentkit.New(t.Context(),
			agentkit.WithModel(&scriptModel{turns: []func(*openresponses.Emitter) error{
				callTurn(agentmemory.InsertTool, `{"scope":"user","content":"`+content+`"}`),
				textTurn("done"),
			}}, "m"),
			agentkit.WithMemory(mem, "user"),
			agentkit.WithMemoryTools(agentmemory.WithInsert("user")),
			agentkit.WithRecorder(fg.Recorder()),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Close()
		spawn := agentsession.NewSpawnID()
		ctx, err := k.StartChild(t.Context(), spawn)
		if err != nil {
			t.Fatal(err)
		}
		if err := prompt(k, ctx); err != nil {
			t.Fatal(err)
		}
		return agentsession.SpawnedSessionID(fg.SessionID(), spawn)
	}
	// writtenBy is the session the journal names for the write of content.
	writtenBy := func(content string) string {
		t.Helper()
		var by []string
		for c, err := range mem.Journal(t.Context(), 0) {
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(c.Entry.Content, content) {
				by = append(by, c.Session)
			}
		}
		if len(by) != 1 {
			t.Fatalf("the journal holds %d writes of %q, want 1", len(by), content)
		}
		return by[0]
	}

	direct := pass("likes tea", func(k *agentkit.Kit, ctx context.Context) error {
		child := agentturn.New(k.Config())
		defer k.AttachChild(child)()
		_, err := child.Prompt(ctx, openresponses.UserText("consolidate memory"))
		return err
	})
	if got := writtenBy("likes tea"); got != direct {
		t.Fatalf("prompted directly, the pass's write is journaled under %q, want the child's session %q", got, direct)
	}

	viaControl := pass("likes oolong", func(k *agentkit.Kit, ctx context.Context) error {
		child := agentturn.New(k.Config(), k.AgentOptions()...)
		defer k.AttachChild(child)()
		c, err := k.Control(child)
		if err != nil {
			return err
		}
		_, err = c.Prompt(ctx, openresponses.UserText("consolidate memory"))
		return err
	})
	if got := writtenBy("likes oolong"); got != fg.SessionID() || got == viaControl {
		t.Fatalf("through Control, the pass's write is journaled under %q, want the foreground's %q (the trap #149 documents), not the child's %q", got, fg.SessionID(), viaControl)
	}
}
