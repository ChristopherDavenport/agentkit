package agentkit_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentpolicy/guard"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	childagent "github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/openresponses"
)

// awsKey is the example key AWS documents, split so a scanner reading
// this file does not match it.
const awsKey = "AKIAIOSFOD" + "NN7EXAMPLE"

func openSession(t *testing.T, store agentsession.Store, id string) *agentsession.Session {
	t.Helper()
	s, err := store.Open(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func configEntries(s *agentsession.Session) []*agentsession.ConfigEntry {
	var out []*agentsession.ConfigEntry
	for _, e := range s.Entries() {
		if c, ok := e.(*agentsession.ConfigEntry); ok {
			out = append(out, c)
		}
	}
	return out
}

func customEntries(s *agentsession.Session, ns string) []*agentsession.CustomEntry {
	var out []*agentsession.CustomEntry
	for _, e := range s.Entries() {
		if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == ns {
			out = append(out, c)
		}
	}
	return out
}

// A session the kit opens records the instructions as the kit's parts,
// with what the layers left out, rather than as one string: a memory
// write is then a delta naming the memory part.
func TestTheSessionRecordsTheInstructionsAsParts(t *testing.T) {
	root := t.TempDir()
	// Two skills of one name: the second is shadowed, which is an
	// omission the record should carry.
	first := skillDir(t, filepath.Join(root, "a"), "digging", "how to dig", "dig")
	second := skillDir(t, filepath.Join(root, "b"), "digging", "another way", "dig")
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "likes-tea", Content: "tea, not coffee"})
	sessions := agentsession.NewMemoryStore()
	model := &scriptModel{}

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithSkills(first, second),
		agentkit.WithMemory(store, "user"),
		agentkit.WithSession(sessions, agentsession.Header{CWD: root}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("hello")); err != nil {
		t.Fatal(err)
	}

	configs := configEntries(openSession(t, sessions, kit.SessionID()))
	if len(configs) == 0 {
		t.Fatal("no config entry")
	}
	var parts, omitted int
	for _, c := range configs {
		parts += len(c.InstructionsParts)
		omitted += len(c.InstructionsOmitted)
	}
	if parts == 0 {
		t.Fatalf("no config entry carries instructions_parts; the recorder took the string")
	}
	if omitted == 0 {
		t.Fatalf("no config entry carries instructions_omitted; the kit omitted %v", kit.Omitted())
	}
}

// guard.Redact rewrites the instructions. Run over each part, it
// rewrites the part the secret is in, so the parts the kit holds are the
// request that was sent and the recorder takes them.
func TestARedactedPartIsThePartThatWasSent(t *testing.T) {
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "deploy", Content: "the key is " + awsKey})
	entryPart := agentmemory.PartID("user", "deploy")
	sessions := agentsession.NewMemoryStore()
	model := &scriptModel{}

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithMemory(store, "user"),
		agentkit.WithGuards(guard.Redact()),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("hello")); err != nil {
		t.Fatal(err)
	}

	req := model.requests()[0]
	if strings.Contains(req.Instructions, awsKey) || !strings.Contains(req.Instructions, "[REDACTED") {
		t.Fatalf("the request was not redacted:\n%s", req.Instructions)
	}
	if _, err := agentsession.ConfigFromRequestParts(req, kit.Parts()...); err != nil {
		t.Fatalf("the parts do not describe the redacted request: %v", err)
	}

	// Every config entry, not only the last: the first is settled at the
	// run's start from the configuration's own instructions, and it is
	// the entry a reader sees first.
	var entry string
	for i, c := range configEntries(openSession(t, sessions, kit.SessionID())) {
		if c.Instructions != nil && strings.Contains(*c.Instructions, awsKey) {
			t.Fatalf("config entry %d carries the key in its instructions", i)
		}
		for _, p := range c.InstructionsParts {
			if strings.Contains(p.Text, awsKey) {
				t.Fatalf("config entry %d carries the key in part %s", i, p.ID)
			}
			if p.ID == entryPart {
				entry = p.Text
			}
		}
	}
	if !strings.Contains(entry, "[REDACTED") {
		t.Fatalf("the recorded memory entry part = %q, want the redacted text", entry)
	}
	if strings.Contains(kit.Config().Instructions, awsKey) {
		t.Fatal("the configuration's own instructions carry the key")
	}

	// Redact gave a reason, so its verdict on the memory entry is on the
	// record, naming the part.
	var named bool
	for _, c := range customEntries(openSession(t, sessions, kit.SessionID()), agentpolicy.VerdictNS) {
		var v struct{ Guard, Subject string }
		if err := json.Unmarshal(c.Data, &v); err != nil {
			t.Fatal(err)
		}
		if v.Guard == "redact" && v.Subject == "instructions/"+entryPart {
			named = true
		}
	}
	if !named {
		t.Fatal("no redact verdict on the memory part is recorded")
	}
}

// The engine's verdicts reach the session under agentpolicy's
// namespace, and the product's observer still sees every one.
func TestTheEnginesVerdictsAreRecorded(t *testing.T) {
	root := t.TempDir()
	skills := skillDir(t, filepath.Join(root, "skills"), "digging", "how to dig", "dig with care")
	sessions := agentsession.NewMemoryStore()
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("skill", `{"name":"digging"}`),
	}}

	var mu sync.Mutex
	var seen []agentpolicy.Verdict
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{Read: []string{agentskill.ToolName}}), nil),
		agentkit.WithVerdictObserver(func(_ context.Context, v agentpolicy.Verdict) {
			mu.Lock()
			seen = append(seen, v)
			mu.Unlock()
		}),
		agentkit.WithSession(sessions, agentsession.Header{CWD: root}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("dig")); err != nil {
		t.Fatal(err)
	}

	recorded := customEntries(openSession(t, sessions, kit.SessionID()), agentpolicy.VerdictNS)
	if len(recorded) == 0 {
		t.Fatal("no verdict is recorded")
	}
	var v struct{ Tool, Action string }
	if err := json.Unmarshal(recorded[0].Data, &v); err != nil {
		t.Fatal(err)
	}
	if v.Tool != agentskill.ToolName || v.Action != "allow" {
		t.Fatalf("recorded verdict = %+v, want the skill call allowed", v)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(recorded) {
		t.Fatalf("the product saw %d verdicts and %d are recorded", len(seen), len(recorded))
	}
}

// A guard's bare allow is not written: the guards run on every part and
// every message, and their silence would outweigh the run.
func TestAGuardsBareAllowIsNotRecorded(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	var seen int
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "test-model"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithGuards(guard.Redact()),
		agentkit.WithVerdictObserver(func(context.Context, agentpolicy.Verdict) { seen++ }),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("the product's observer saw no guard verdict")
	}
	if got := customEntries(openSession(t, sessions, kit.SessionID()), agentpolicy.VerdictNS); len(got) != 0 {
		t.Fatalf("%d bare allows recorded, want none", len(got))
	}
}

// A person's approval is recorded as a person's, when the front says so
// with WithBy as the README does.
func TestAHumanApprovalIsRecordedWithItsBy(t *testing.T) {
	root := t.TempDir()
	skills := skillDir(t, filepath.Join(root, "skills"), "digging", "how to dig", "dig with care")
	sessions := agentsession.NewMemoryStore()
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("skill", `{"name":"digging"}`),
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{}), nil),
		agentkit.WithSession(sessions, agentsession.Header{CWD: root}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	end, err := agent.Prompt(t.Context(), openresponses.UserText("dig a hole"))
	if err != nil {
		t.Fatal(err)
	}
	if len(end.Pending) != 1 {
		t.Fatalf("pending = %d, want the held call", len(end.Pending))
	}
	answers, err := kit.Engine().Release(t.Context(), end,
		agentturn.Approve(end.Pending[0].Call.CallID).WithBy(agentpolicy.ByHuman))
	if err != nil {
		t.Fatal(err)
	}
	if end, err = agent.Resume(t.Context(), answers...); err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonDone {
		t.Fatalf("reason = %q, want done", end.Reason)
	}

	var proceed *agentsession.DecisionEntry
	for _, e := range openSession(t, sessions, kit.SessionID()).Entries() {
		if d, ok := e.(*agentsession.DecisionEntry); ok && d.Verdict == "proceed" {
			proceed = d
		}
	}
	if proceed == nil {
		t.Fatal("no proceed decision is recorded")
	}
	if proceed.By != agentpolicy.ByHuman {
		t.Fatalf("the approval is recorded by %q, want %q", proceed.By, agentpolicy.ByHuman)
	}
}

// A child agent's memory write names the child's session, not the
// parent's: the kit runs the child under the recorder's ChildContext and
// bridges its ID to agentmemory's key.
func TestAChildsMemoryWriteNamesTheChildsSession(t *testing.T) {
	store := memStore(t)
	sessions := agentsession.NewMemoryStore()

	var wroteAs string
	save := agenttool.New("save", "save a note",
		func(ctx context.Context, _ agenttool.NoArgs) (string, error) {
			c, err := store.Put(ctx, agentmemory.Entry{Scope: "user", Name: "note", Content: "from the child"})
			if err != nil {
				return "", err
			}
			wroteAs = c.Session
			return "saved", nil
		})
	child := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("save", `{}`)}}
	parent := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("explore", `{"input":"save a note"}`)}}

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(parent, "test-model"),
		agentkit.WithMemory(store, "user"),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
		agentkit.WithChildAgent(agentturn.Config{
			Name:        "explore",
			Description: "delegate",
			Model:       child,
			ModelName:   "test-model",
			Tools:       []agenttool.Tool{save},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if wroteAs == "" {
		t.Fatal("the child's write names no session")
	}
	if wroteAs == kit.SessionID() {
		t.Fatalf("the child's write names the parent's session %q", wroteAs)
	}
	if want := agentsession.SubsessionID(kit.SessionID(), "call-explore"); wroteAs != want {
		t.Fatalf("the child's write names %q, want the child's session %q", wroteAs, want)
	}
}

// A kit built onto a recorder it did not open records into that
// recorder, opens nothing, and leaves the attaching to the owner.
func TestAKitRecordsIntoARecorderItWasGiven(t *testing.T) {
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "likes-tea", Content: "tea, not coffee"})
	sessions := agentsession.NewMemoryStore()
	rec, _, err := session.Start(t.Context(), sessions, agentsession.Header{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "test-model"),
		agentkit.WithMemory(store, "user"),
		agentkit.WithRecorder(rec),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	if kit.Recorder() != rec || kit.SessionID() != rec.SessionID() {
		t.Fatal("the kit does not hold the recorder it was given")
	}
	if kit.Session() != nil {
		t.Fatal("the kit opened a session")
	}
	if kit.Config().ToolRecorder == nil {
		t.Fatal("ToolRecorder is not bound to the recorder")
	}

	agent := agentturn.New(kit.Config())
	defer rec.Attach(agent)()
	// The owner attached it; the kit's Attach must not subscribe twice.
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("hello")); err != nil {
		t.Fatal(err)
	}

	s := openSession(t, sessions, rec.SessionID())
	if got := customEntries(s, agentmemory.ManifestNS); len(got) != 1 {
		t.Fatalf("manifest entries = %d, want 1 in the recorder's session", len(got))
	}
	var runs int
	for _, e := range s.Entries() {
		if r, ok := e.(*agentsession.RunEntry); ok && r.Phase == agentsession.RunStart {
			runs++
		}
	}
	if runs != 1 {
		t.Fatalf("run starts = %d, want 1; a second subscription writes every event twice", runs)
	}
	var n int
	for _, err := range sessions.List(t.Context(), agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("sessions = %d, want the recorder's alone", n)
	}
}

func TestWithRecorderAndASessionAreRefused(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	rec, _, err := session.Start(t.Context(), sessions, agentsession.Header{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithRecorder(rec),
		agentkit.WithSession(sessions, agentsession.Header{}),
	)
	if err == nil || !strings.Contains(err.Error(), "WithRecorder") {
		t.Fatalf("err = %v, want one naming WithRecorder", err)
	}
}

// PartsFor has the signature session.WithInstructionsParts takes, so a
// recorder opened before the kit takes the kit's parts.
func TestARecorderOpenedElsewhereTakesTheKitsParts(t *testing.T) {
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "likes-tea", Content: "tea, not coffee"})
	sessions := agentsession.NewMemoryStore()
	var kit *agentkit.Kit
	rec, _, err := session.Start(t.Context(), sessions, agentsession.Header{CWD: t.TempDir()},
		session.WithInstructionsParts(func(ctx context.Context, req openresponses.Request) ([]agentsession.InstructionPart, []agentsession.OmittedPart) {
			return kit.PartsFor(ctx, req)
		}))
	if err != nil {
		t.Fatal(err)
	}
	kit, err = agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "test-model"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithMemory(store, "user"),
		agentkit.WithRecorder(rec),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	defer rec.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	var parts int
	for _, c := range configEntries(openSession(t, sessions, rec.SessionID())) {
		parts += len(c.InstructionsParts)
	}
	if parts == 0 {
		t.Fatal("the recorder did not take the kit's parts")
	}

	if p, o := kit.PartsFor(t.Context(), openresponses.Request{Instructions: "something else"}); p != nil || o != nil {
		t.Fatalf("PartsFor a request the parts do not join to = %v, %v; want nil", p, o)
	}
}

// A product that passes its own observer through WithPolicy's options
// keeps both: agentpolicy v0.0.6 keeps every observer, so the kit's
// recording does not depend on which option the product reached for.
func TestAnObserverPassedToWithPolicyRunsBesideTheRecording(t *testing.T) {
	root := t.TempDir()
	skills := skillDir(t, filepath.Join(root, "skills"), "digging", "how to dig", "dig with care")
	sessions := agentsession.NewMemoryStore()
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("skill", `{"name":"digging"}`),
	}}

	var mu sync.Mutex
	var seen int
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{Read: []string{agentskill.ToolName}}), nil,
			agentpolicy.WithObserver(func(context.Context, agentpolicy.Verdict) {
				mu.Lock()
				seen++
				mu.Unlock()
			})),
		agentkit.WithSession(sessions, agentsession.Header{CWD: root}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("dig")); err != nil {
		t.Fatal(err)
	}

	recorded := customEntries(openSession(t, sessions, kit.SessionID()), agentpolicy.VerdictNS)
	if len(recorded) == 0 {
		t.Fatal("the product's observer replaced the kit's: no verdict is recorded")
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != len(recorded) {
		t.Fatalf("the product's observer saw %d verdicts and %d are recorded", seen, len(recorded))
	}
}

// callTurnID is callTurn with a call ID of the test's choosing, for a
// script that calls one tool twice.
func callTurnID(id, name, args string) func(*openresponses.Emitter) error {
	return func(e *openresponses.Emitter) error {
		w, err := e.FunctionCall(id, name)
		if err != nil {
			return err
		}
		if err := w.Arguments(args); err != nil {
			return err
		}
		return w.Close()
	}
}

// A handoff between two kits over one session: a recorder the host
// opens with PartsFrom records each agent's instructions as that
// agent's parts, on both sides of the handoff. (#17)
func TestAHandoffBetweenTwoKitsRecordsBothAsParts(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	var triage, billing *agentkit.Kit
	rec, _, err := session.Start(t.Context(), sessions, agentsession.Header{CWD: t.TempDir()},
		session.WithInstructionsParts(func(ctx context.Context, req openresponses.Request) ([]agentsession.InstructionPart, []agentsession.OmittedPart) {
			return agentkit.PartsFrom(triage, billing)(ctx, req)
		}))
	if err != nil {
		t.Fatal(err)
	}
	build := func(prompt string) *agentkit.Kit {
		kit, err := agentkit.New(t.Context(),
			agentkit.WithModel(&scriptModel{}, "test-model"),
			agentkit.WithInstructions(prompt),
			agentkit.WithMemory(memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: prompt + " remembers"}), "user"),
			agentkit.WithRecorder(rec),
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kit.Close() })
		return kit
	}
	triage, billing = build("You triage."), build("You bill.")

	agent := agentturn.New(triage.Config())
	defer rec.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("my invoice is wrong")); err != nil {
		t.Fatal(err)
	}
	if err := agent.SetConfig(billing.Config()); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("it says 40, not 4")); err != nil {
		t.Fatal(err)
	}

	var sawTriage, sawBilling bool
	for i, c := range configEntries(openSession(t, sessions, rec.SessionID())) {
		if c.Instructions != nil && *c.Instructions != "" && len(c.InstructionsParts) == 0 {
			t.Fatalf("config entry %d records the instructions as one string", i)
		}
		for _, p := range c.InstructionsParts {
			sawTriage = sawTriage || strings.Contains(p.Text, "You triage.")
			sawBilling = sawBilling || strings.Contains(p.Text, "You bill.")
		}
	}
	if !sawTriage || !sawBilling {
		t.Fatalf("parts recorded: triage %v, billing %v; want both agents'", sawTriage, sawBilling)
	}
}

// A kit built under a parent's recorder, inside the parent's
// WithDeferredTools, is called twice; each call is a child session of
// its own, and each carries the memory manifest its run was shown,
// where the kit wrote it to the first alone. (#20)
func TestEachChildSessionCarriesItsOwnMemoryManifest(t *testing.T) {
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "likes-tea", Content: "tea, not coffee"})
	sessions := agentsession.NewMemoryStore()
	parent := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurnID("call-1", "explore", `{"input":"one"}`),
		callTurnID("call-2", "explore", `{"input":"two"}`),
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(parent, "test-model"),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
		agentkit.WithDeferredTools(func(k *agentkit.Kit) []agenttool.Tool {
			child, err := agentkit.New(t.Context(),
				agentkit.WithName("explore", "delegate"),
				agentkit.WithModel(&scriptModel{}, "test-model"),
				agentkit.WithMemory(store, "user"),
				agentkit.WithRecorder(k.Recorder()),
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = child.Close() })
			rec := k.Recorder()
			return []agenttool.Tool{childagent.New(child.Config(),
				childagent.WithObserver(rec.Observe),
				childagent.WithRunContext(rec.ChildContext))}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"call-1", "call-2"} {
		id := agentsession.SubsessionID(kit.SessionID(), call)
		if got := customEntries(openSession(t, sessions, id), agentmemory.ManifestNS); len(got) == 0 {
			t.Errorf("the child session of %s holds no memory manifest", call)
		}
	}
}

// A small memory write under a large block is recorded as the entry's
// part and the summary, not as the whole block again. (#21)
func TestAMemoryWriteIsRecordedAsThePartsItChanged(t *testing.T) {
	store := memStore(t,
		agentmemory.Entry{Scope: "user", Name: "big-one", Content: strings.Repeat("a", 4000)},
		agentmemory.Entry{Scope: "user", Name: "big-two", Content: strings.Repeat("b", 4000)},
	)
	sessions := agentsession.NewMemoryStore()
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentmemory.SaveTool, `{"scope":"user","name":"travel","content":"window seat"}`),
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithMemory(store, "user"),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	end, err := agent.Prompt(t.Context(), openresponses.UserText("I like the window"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Err != nil {
		t.Fatal(end.Err)
	}

	entries := configEntries(openSession(t, sessions, kit.SessionID()))
	if len(entries) < 2 {
		t.Fatalf("config entries = %d, want the first and the delta the write caused", len(entries))
	}
	last := entries[len(entries)-1]
	var carried []string
	for _, p := range last.InstructionsParts {
		if p.Text != "" {
			carried = append(carried, p.ID)
		}
	}
	slices.Sort(carried)
	want := []string{agentmemory.SummaryPartID, agentmemory.PartID("user", "travel")}
	slices.Sort(want)
	if !slices.Equal(carried, want) {
		t.Fatalf("the write's delta carries the text of %v, want only %v", carried, want)
	}
}

// memory_save is based on the block the model was shown: a write
// another session makes while the model composes is reported as lost,
// where the save used to read the entry again and report nothing. (#22)
func TestASaveOverAWriteAfterTheRenderIsALostUpdate(t *testing.T) {
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "profile", Content: "prefers Python"})
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		func(e *openresponses.Emitter) error {
			// Another session writes the entry after the render, while the
			// model is composing its rewrite of it.
			other := agentmemory.WithSession(context.Background(), "telegram")
			if _, err := store.Put(other, agentmemory.Entry{Scope: "user", Name: "profile", Content: "prefers Go"}); err != nil {
				return err
			}
			return callTurn(agentmemory.SaveTool, `{"scope":"user","name":"profile","content":"prefers Python, likes tea"}`)(e)
		},
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithMemory(store, "user"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("remember I like tea")); err != nil {
		t.Fatal(err)
	}
	lost, err := agentmemory.LostUpdates(t.Context(), store, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 {
		t.Fatalf("lost updates = %d, want the write the save discarded", len(lost))
	}
}
