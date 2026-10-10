package agentkit_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type noArgs struct{}

func policyRules(t *testing.T, s string) []agentpolicy.Rule {
	t.Helper()
	r, err := agentpolicy.ParseRules(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// controlled builds a kit that records into a memory store, the agent
// over it attached, and its Control.
func controlled(t *testing.T, opts ...agentkit.Option) (*agentkit.Kit, *agentkit.Control, agentsession.Store) {
	t.Helper()
	store := agentsession.NewMemoryStore()
	opts = append(opts, agentkit.WithSession(store, agentsession.Header{Records: agentsession.AllRecords}))
	kit, err := agentkit.New(t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kit.Close() })
	agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
	t.Cleanup(kit.Attach(agent))
	ctl, err := kit.Control(agent)
	if err != nil {
		t.Fatal(err)
	}
	return kit, ctl, store
}

// counting is a tool that counts its runs.
func counting(name string, ran *atomic.Int32) agenttool.Tool {
	return agenttool.New(name, "does "+name, func(context.Context, noArgs) (string, error) {
		ran.Add(1)
		return name + " done", nil
	})
}

// Of two calls in a batch the policy asks about one; the engine holds
// the other. Permissions lists the asked one alone, with the policy's
// question, and one Resume answering it releases both.
func TestControlReleasesTheCallTheEngineHeld(t *testing.T) {
	var ranDanger, ranSafe atomic.Int32
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{twoCalls("danger", `{}`, "safe", `{}`)}}
	_, ctl, _ := controlled(t,
		agentkit.WithModel(model, "test-model"),
		agentkit.WithTools(counting("danger", &ranDanger), counting("safe", &ranSafe)),
		agentkit.WithPolicy(agentpolicy.Policy{Ask: policyRules(t, "danger"), Default: agentpolicy.Allow()}, nil),
	)
	end, err := ctl.Prompt(t.Context(), openresponses.UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 2 {
		t.Fatalf("end = %q with %d pending, want input_required with both held", end.Reason, len(end.Pending))
	}
	perms := ctl.Permissions(end)
	if len(perms) != 1 || perms[0].Call.Name != "danger" || perms[0].Reason == "" {
		t.Fatalf("permissions = %+v, want danger alone, with the policy's question", perms)
	}
	end, err = ctl.Resume(t.Context(), agentturn.Approve(perms[0].Call.CallID).WithBy(agentpolicy.ByHuman))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonDone || ranDanger.Load() != 1 || ranSafe.Load() != 1 {
		t.Fatalf("end = %q, danger ran %d, safe ran %d; want done with both run once", end.Reason, ranDanger.Load(), ranSafe.Load())
	}
}

// An answer Resume would refuse is refused before the engine releases
// anything, so the held calls stay held and can still be answered.
func TestControlRefusesABadAnswerBeforeTheRelease(t *testing.T) {
	var ranDanger, ranSafe atomic.Int32
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{twoCalls("danger", `{}`, "safe", `{}`)}}
	_, ctl, _ := controlled(t,
		agentkit.WithModel(model, "test-model"),
		agentkit.WithTools(counting("danger", &ranDanger), counting("safe", &ranSafe)),
		agentkit.WithPolicy(agentpolicy.Policy{Ask: policyRules(t, "danger"), Default: agentpolicy.Allow()}, nil),
	)
	if _, err := ctl.Prompt(t.Context(), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		answers []agentturn.Answer
	}{
		{name: "not pending", answers: []agentturn.Answer{agentturn.Approve("call-nothing")}},
		{name: "twice", answers: []agentturn.Answer{agentturn.Approve("call-danger"), agentturn.Approve("call-danger")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ctl.Resume(t.Context(), tc.answers...); err == nil {
				t.Fatal("Resume took the answers")
			}
			if n := len(ctl.State().Pending); n != 2 {
				t.Fatalf("%d calls pending after the refusal, want both still held", n)
			}
		})
	}
	if end, err := ctl.Resume(t.Context(), agentturn.Approve("call-danger").WithBy(agentpolicy.ByHuman)); err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("answering after the refusals: %v, %+v", err, end)
	}
}

// A run through Control carries the kit's session: a tool sees the
// session's ID on its context, for the model calls and for memory.
func TestControlRunsCarryTheKitsSession(t *testing.T) {
	var seen, memory string
	probe := agenttool.New("probe", "reads its context", func(ctx context.Context, _ noArgs) (string, error) {
		seen, memory = session.SessionIDFromContext(ctx), agentmemory.SessionFrom(ctx)
		return "ok", nil
	})
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("probe", `{}`)}}
	kit, ctl, _ := controlled(t, agentkit.WithModel(model, "test-model"), agentkit.WithTools(probe))
	if _, err := ctl.Prompt(t.Context(), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if want := kit.SessionID(); seen != want || memory != want {
		t.Fatalf("the tool saw session %q and memory session %q, want %q for both", seen, memory, want)
	}
}

// A queued input is a queued entry in the store when Queue returns,
// with no run going to write it.
func TestControlQueueIsOnTheRecordWhenItReturns(t *testing.T) {
	kit, ctl, store := controlled(t, agentkit.WithModel(&scriptModel{}, "test-model"))
	for _, mode := range []agentturn.QueueMode{agentturn.QueueSteer, agentturn.QueueFollowUp} {
		if err := ctl.Queue(t.Context(), mode, openresponses.UserText("later, "+string(mode))); err != nil {
			t.Fatal(err)
		}
	}
	s := openSession(t, store, kit.SessionID())
	var queued int
	for _, e := range s.Entries() {
		if q, ok := e.(*agentsession.QueuedEntry); ok && q != nil {
			queued++
		}
	}
	if queued != 2 {
		t.Fatalf("%d queued entries, want the steer and the follow-up", queued)
	}
}

// A nested call the policy asks about is a Question event, answered
// with Reply. A refusal's note reaches the model with the refusal and
// is on the record's answer entry.
func TestControlAsksANestedCallAsAQuestion(t *testing.T) {
	cases := []struct {
		name   string
		answer agenttool.Answer
		ran    int32
	}{
		{name: "declined, with a note", answer: agenttool.Answer{Action: agenttool.ActionDecline, Note: "use the fixture"}},
		{name: "accepted", answer: agenttool.Answer{Action: agenttool.ActionAccept}, ran: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ranBash atomic.Int32
			eval := agenttool.New("eval", "runs code", func(ctx context.Context, _ noArgs) (string, error) {
				if _, err := agentturn.Invoke(ctx, "bash", json.RawMessage(`{}`)); err != nil {
					return "the nested call failed: " + err.Error(), nil
				}
				return "ran", nil
			})
			model := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("eval", `{}`)}}
			kit, ctl, store := controlled(t,
				agentkit.WithModel(model, "test-model"),
				agentkit.WithTools(eval, counting("bash", &ranBash)),
				agentkit.WithPolicy(agentpolicy.Policy{Ask: policyRules(t, "bash"), Default: agentpolicy.Allow()}, nil),
			)
			var questions []*agentturn.Question
			var closed int
			defer ctl.Subscribe(func(_ context.Context, ev agentturn.Event) error {
				switch e := ev.(type) {
				case *agentturn.Question:
					questions = append(questions, e)
					return ctl.Reply(e.ID, tc.answer)
				case *agentturn.QuestionClosed:
					closed++
				}
				return nil
			})()
			end, err := ctl.Prompt(t.Context(), openresponses.UserText("go"))
			if err != nil {
				t.Fatal(err)
			}
			if end.Reason != agentturn.ReasonDone {
				t.Fatalf("end = %q, want done", end.Reason)
			}
			if len(questions) != 1 || closed != 1 || questions[0].Call == nil || questions[0].Call.Name != "bash" {
				t.Fatalf("questions = %+v, %d closed; want one about bash, closed", questions, closed)
			}
			if ranBash.Load() != tc.ran {
				t.Fatalf("bash ran %d times, want %d", ranBash.Load(), tc.ran)
			}
			if note := tc.answer.Note; note != "" {
				if out := lastToolOutput(model); !strings.Contains(out, note) {
					t.Errorf("the model was told %q, which leaves out the note %q", out, note)
				}
			}
			s := openSession(t, store, kit.SessionID())
			var answer *session.Elicitation
			for _, c := range customEntries(s, session.ElicitationNS) {
				var e session.Elicitation
				if err := json.Unmarshal(c.Data, &e); err != nil {
					t.Fatal(err)
				}
				if e.Phase == session.ElicitationAnswer {
					answer = &e
				}
			}
			if answer == nil || answer.Action != string(tc.answer.Action) || answer.Note != tc.answer.Note || answer.By != agentsession.ByHuman {
				t.Fatalf("answer entry = %+v, want %s by human with note %q", answer, tc.answer.Action, tc.answer.Note)
			}
		})
	}
}

// A kit built with an elicitor of its own answers in process, so
// Control leaves the agent's config alone.
func TestControlKeepsTheKitsOwnElicitor(t *testing.T) {
	asked := false
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithToolElicitor(agentpolicy.ByHuman, func(context.Context, agenttool.Elicitation) (agenttool.Answer, error) {
			asked = true
			return agenttool.Answer{Action: agenttool.ActionCancel}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	agent := agentturn.New(kit.Config())
	if _, err := kit.Control(agent); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Config().ToolElicitor(t.Context(), agenttool.Elicitation{Message: "?"}); err != nil || !asked {
		t.Fatalf("the agent's elicitor is not the kit's: asked %v, %v", asked, err)
	}
}

// Moving the head back to before a skill read ends its grant, and
// moving forward to after it grants it again; the agent's transcript
// follows the head.
func TestControlContinueFromMovesTheSkillGrants(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "danger")
	var ran atomic.Int32
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn(agentskill.ToolName, `{"name":"digging"}`)}}
	kit, ctl, store := controlled(t,
		agentkit.WithModel(model, "test-model"),
		agentkit.WithTools(counting("danger", &ran)),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Policy{Allow: policyRules(t, "skill"), Default: agentpolicy.Ask()}, nil),
		agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
			return agentpolicy.Source{Name: "skill:" + sk.Name, Path: sk.Location, Trusted: true}
		}),
		agentkit.WithSkillGrantScope(),
	)
	if _, err := ctl.Prompt(t.Context(), openresponses.UserText("learn")); err != nil {
		t.Fatal(err)
	}
	grants := func() int {
		ctx := agentpolicy.ContextWithGrantScope(t.Context(), kit.GrantScope(ctl.RunContext(t.Context())))
		return len(kit.Engine().GrantsFor(ctx))
	}
	if grants() == 0 {
		t.Fatal("no grant after the read")
	}
	s := openSession(t, store, kit.SessionID())
	var user, read string
	for _, e := range s.Entries() {
		it, ok := e.(*agentsession.ItemEntry)
		if !ok {
			continue
		}
		switch item := it.Item.(type) {
		case *openresponses.Message:
			if item.Role == openresponses.RoleUser && user == "" {
				user = e.Base().ID
			}
		case *openresponses.FunctionCallOutput:
			read = e.Base().ID
		}
	}
	if user == "" || read == "" {
		t.Fatalf("no user message (%q) or skill output (%q) on the record", user, read)
	}

	if err := ctl.ContinueFrom(t.Context(), user); err != nil {
		t.Fatalf("continue from the prompt: %v", err)
	}
	if n := grants(); n != 0 {
		t.Fatalf("%d grants after moving to before the read, want 0", n)
	}
	if n := len(ctl.State().Transcript); n != 1 {
		t.Fatalf("transcript has %d items after moving to the prompt, want the prompt alone", n)
	}
	if err := ctl.ContinueFrom(t.Context(), read); err != nil {
		t.Fatalf("continue from the read's output: %v", err)
	}
	if grants() == 0 {
		t.Fatal("the grant was not made again moving to after the read")
	}
}

// A head move to a leaf label, or to nothing, is refused before
// anything moves.
func TestControlContinueFromRefusesWhereTheHeadCannotRest(t *testing.T) {
	_, ctl, _ := controlled(t, agentkit.WithModel(&scriptModel{}, "test-model"))
	if _, err := ctl.Prompt(t.Context(), openresponses.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	before := len(ctl.State().Transcript)
	for _, id := range []string{"", "no-such-entry"} {
		if err := ctl.ContinueFrom(t.Context(), id); err == nil {
			t.Errorf("ContinueFrom(%q) moved the head", id)
		}
	}
	if n := len(ctl.State().Transcript); n != before {
		t.Fatalf("transcript has %d items after the refusals, want %d", n, before)
	}
}

// Under WithQuestionEvents an MCP server's elicitation is a Question
// through Control, answered with Reply: the server reads the answer,
// and the record keeps it with what the user said, which MCP does not
// carry to the server.
func TestControlPutsAnMCPServersQuestionAsAnEvent(t *testing.T) {
	cases := []struct {
		name   string
		answer agenttool.Answer
	}{
		{name: "accepted", answer: agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{"ok":true}`)}},
		{name: "declined, with a note", answer: agenttool.Answer{Action: agenttool.ActionDecline, Note: "not on a Friday"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "0"}, nil)
			sdk.AddTool(srv, &sdk.Tool{Name: "deploy", Description: "deploy after asking"},
				func(_ context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
					answer, ok := req.Params.InputResponses["confirm"].(*sdk.ElicitResult)
					if !ok {
						return &sdk.CallToolResult{InputRequests: sdk.InputRequestMap{"confirm": &sdk.ElicitParams{
							Message:         "Deploy to production?",
							RequestedSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
						}}}, nil, nil
					}
					return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "server read " + answer.Action}}}, nil, nil
				})
			client, server := sdk.NewInMemoryTransports()
			ss, err := srv.Connect(t.Context(), server, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ss.Close() })

			model := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("deploy", `{}`)}}
			kit, ctl, store := controlled(t,
				agentkit.WithModel(model, "test-model"),
				agentkit.WithMCPTransport(client),
				agentkit.WithQuestionEvents(),
			)
			var asked []string
			defer ctl.Subscribe(func(_ context.Context, ev agentturn.Event) error {
				if q, ok := ev.(*agentturn.Question); ok {
					asked = append(asked, q.Elicitation.Message)
					return ctl.Reply(q.ID, tc.answer)
				}
				return nil
			})()
			if _, err := ctl.Prompt(t.Context(), openresponses.UserText("ship it")); err != nil {
				t.Fatal(err)
			}
			if len(asked) != 1 || asked[0] != "Deploy to production?" {
				t.Fatalf("questions = %q, want the server's one", asked)
			}
			if out := lastToolOutput(model); !strings.Contains(out, "server read "+string(tc.answer.Action)) {
				t.Fatalf("the tool's output = %q, want the server to have read %s", out, tc.answer.Action)
			}
			var answer *session.Elicitation
			for _, c := range customEntries(openSession(t, store, kit.SessionID()), session.ElicitationNS) {
				var e session.Elicitation
				if err := json.Unmarshal(c.Data, &e); err != nil {
					t.Fatal(err)
				}
				if e.Phase == session.ElicitationAnswer {
					answer = &e
				}
			}
			if answer == nil || answer.Action != string(tc.answer.Action) || answer.Note != tc.answer.Note {
				t.Fatalf("answer entry = %+v, want %s with note %q", answer, tc.answer.Action, tc.answer.Note)
			}
		})
	}
}

// WithQuestionEvents and WithToolElicitor name two answerers for one
// question.
func TestQuestionEventsBesideAToolElicitorIsRefused(t *testing.T) {
	_, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithQuestionEvents(),
		agentkit.WithToolElicitor(agentpolicy.ByHuman, func(context.Context, agenttool.Elicitation) (agenttool.Answer, error) {
			return agenttool.Answer{Action: agenttool.ActionCancel}, nil
		}),
	)
	if err == nil {
		t.Fatal("New took both")
	}
}

// Once Control has installed the elicitor, the kit's config carries it,
// so a config applied again after a skill reload still puts a nested
// call's question as an event. A second agent cannot take the kit's
// questions over.
func TestControlsElicitorSurvivesAReappliedConfig(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "bash")
	var ranBash atomic.Int32
	eval := agenttool.New("eval", "runs code", func(ctx context.Context, _ noArgs) (string, error) {
		if _, err := agentturn.Invoke(ctx, "bash", json.RawMessage(`{}`)); err != nil {
			return "the nested call failed: " + err.Error(), nil
		}
		return "ran", nil
	})
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("eval", `{}`)}}
	kit, ctl, _ := controlled(t,
		agentkit.WithModel(model, "test-model"),
		agentkit.WithTools(eval, counting("bash", &ranBash)),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Policy{Ask: policyRules(t, "bash"), Default: agentpolicy.Allow()}, nil),
	)
	if kit.Config().ToolElicitor == nil {
		t.Fatal("the kit's config carries no elicitor after Control")
	}
	if err := kit.ReloadSkills(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := ctl.Agent().SetConfig(kit.Config()); err != nil {
		t.Fatal(err)
	}
	var questions int
	defer ctl.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		if q, ok := ev.(*agentturn.Question); ok {
			questions++
			return ctl.Reply(q.ID, agenttool.Answer{Action: agenttool.ActionAccept})
		}
		return nil
	})()
	if _, err := ctl.Prompt(t.Context(), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if questions != 1 || ranBash.Load() != 1 {
		t.Fatalf("%d questions, bash ran %d times; want the nested call asked and run once", questions, ranBash.Load())
	}

	if _, err := kit.Control(agentturn.New(kit.Config())); err == nil {
		t.Fatal("a second agent took the kit's questions")
	}
	if _, err := kit.Control(ctl.Agent()); err != nil {
		t.Fatalf("Control again for the same agent: %v", err)
	}
}
