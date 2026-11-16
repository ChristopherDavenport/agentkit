package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// summaries is the summary requests a model was sent: those with no
// instructions, since every turn's carries the kit's.
func summaries(m *scriptModel) []openresponses.Request {
	var out []openresponses.Request
	for _, r := range m.requests() {
		if r.Instructions == "" {
			out = append(out, r)
		}
	}
	return out
}

// foldCalls is the fold member of each compaction the session records.
func foldCalls(t *testing.T, s *agentsession.Session) []session.FoldCall {
	t.Helper()
	var out []session.FoldCall
	for _, e := range s.Entries() {
		c, ok := e.(*agentsession.CompactionEntry)
		if !ok {
			continue
		}
		var f session.FoldCall
		if err := json.Unmarshal(c.Unknown[session.FoldMember], &f); err != nil {
			t.Fatalf("the compaction's fold member: %v", err)
		}
		out = append(out, f)
	}
	return out
}

// compactingKit is a kit whose agent the test switches: a summary
// model beside the agent's, a large budget so nothing folds unasked,
// and a session to record the folds.
func compactingKit(t *testing.T, model *scriptModel, extra ...agentkit.Option) (*agentkit.Kit, *agentturn.Agent, *[]compact.Fold) {
	t.Helper()
	var folds []compact.Fold
	opts := append([]agentkit.Option{
		agentkit.WithModel(model, "big"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithReasoning(openresponses.ReasoningConfig{Effort: openresponses.ReasoningEffortHigh}),
		agentkit.WithFoldObserver(func(_ context.Context, f compact.Fold) { folds = append(folds, f) }),
		agentkit.WithSession(agentsession.NewMemoryStore(), agentsession.Header{}),
	}, extra...)
	kit, err := agentkit.New(t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kit.Close() })
	agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
	t.Cleanup(kit.Attach(agent))
	return kit, agent, &folds
}

func switchModel(t *testing.T, kit *agentkit.Kit, agent *agentturn.Agent, name string, effort openresponses.ReasoningEffort) {
	t.Helper()
	cfg := kit.Config()
	cfg.ModelName = name
	cfg.Reasoning = openresponses.ReasoningConfig{Effort: effort}
	if err := agent.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// After the agent is switched to another model, the next summary is
// asked of the model the request being folded for names, under that
// request's reasoning, not the model and reasoning the kit was built
// with, and the fold, recorded, names it. (#143, agentturn#318)
func TestASummaryFollowsTheAgentsModel(t *testing.T) {
	model := &scriptModel{}
	kit, agent, folds := compactingKit(t, model,
		agentkit.WithCompaction(1_000_000, compact.WithKeepLast(1), compact.WithMinFold(0)))
	ctl, err := kit.Control(agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two"} {
		if _, err := ctl.Prompt(t.Context(), openresponses.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	switchModel(t, kit, agent, "small", openresponses.ReasoningEffortLow)
	if err := ctl.Compact(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.Prompt(t.Context(), openresponses.UserText("three")); err != nil {
		t.Fatal(err)
	}
	sum := summaries(model)
	if len(sum) != 1 {
		t.Fatalf("summary requests = %d, want 1", len(sum))
	}
	if sum[0].Model != "small" || sum[0].Reasoning.Effort != openresponses.ReasoningEffortLow {
		t.Errorf("the summary was asked of %q at effort %q, want the switched model small at low", sum[0].Model, sum[0].Reasoning.Effort)
	}
	if len(*folds) != 1 || (*folds)[0].Model != "small" {
		t.Fatalf("folds = %+v, want one naming small", *folds)
	}
	if got := foldCalls(t, kit.Session()); len(got) != 1 || got[0].Model != "small" || got[0].Budget == nil || *got[0].Budget != 1_000_000 {
		t.Errorf("recorded fold = %+v, want model small under budget 1000000", got)
	}
}

// Kit.SetCompactionBudget changes when the next fold happens, mid
// session: under a budget nothing reached, a cut to one the transcript
// is past folds on the next request, and the fold, recorded, carries
// the new budget. The kit writes no entry for the change, which is the
// host's setting. Without compaction there is no budget to set.
// (#143, agentturn#318)
func TestSetCompactionBudgetMovesTheNextFold(t *testing.T) {
	model := &scriptModel{}
	kit, agent, folds := compactingKit(t, model,
		agentkit.WithCompaction(1_000_000, compact.WithKeepLast(1), compact.WithMinFold(0)))
	for _, text := range []string{"one", "two"} {
		if _, err := agent.Prompt(t.Context(), openresponses.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	if len(*folds) != 0 {
		t.Fatalf("folded %d times under the large budget", len(*folds))
	}
	before := len(kit.Session().Entries())
	if err := kit.SetCompactionBudget(1); err != nil {
		t.Fatal(err)
	}
	if after := len(kit.Session().Entries()); after != before {
		t.Fatalf("SetCompactionBudget wrote %d entries; the setting is the host's to record", after-before)
	}
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("three")); err != nil {
		t.Fatal(err)
	}
	if len(*folds) == 0 || (*folds)[0].Budget != 1 || (*folds)[0].Forced {
		t.Fatalf("folds = %+v, want one the cut budget made", *folds)
	}
	if got := foldCalls(t, kit.Session()); len(got) == 0 || got[0].Budget == nil || *got[0].Budget != 1 {
		t.Errorf("recorded fold = %+v, want budget 1", got)
	}

	plain, err := agentkit.New(t.Context(), agentkit.WithModel(&scriptModel{}, "m"))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if err := plain.SetCompactionBudget(10); !errors.Is(err, agentkit.ErrNoCompaction) {
		t.Fatalf("SetCompactionBudget without compaction = %v, want ErrNoCompaction", err)
	}
}

// A model the product chose for the summary stays after the agent is
// switched: a compact.WithModel in WithCompaction's options names it,
// and WithCompactionModel's streamer writes it under the name the kit
// was built with, unless a compact.WithModel names another. (#143)
func TestAChosenSummaryModelStaysAfterASwitch(t *testing.T) {
	for _, c := range []struct {
		name   string
		cheap  bool
		opts   []compact.Option
		want   string
		effort openresponses.ReasoningEffort
	}{
		{"compact.WithModel", false, []compact.Option{compact.WithModel("pinned")}, "pinned", ""},
		{"WithCompactionModel", true, nil, "big", ""},
		{"WithCompactionModel and compact.WithModel", true, []compact.Option{compact.WithModel("cheap")}, "cheap", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			model, cheap := &scriptModel{}, &scriptModel{}
			extra := []agentkit.Option{agentkit.WithCompaction(1_000_000, append([]compact.Option{compact.WithKeepLast(1), compact.WithMinFold(0)}, c.opts...)...)}
			if c.cheap {
				extra = append(extra, agentkit.WithCompactionModel(cheap))
			}
			kit, agent, _ := compactingKit(t, model, extra...)
			ctl, err := kit.Control(agent)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"one", "two"} {
				if _, err := ctl.Prompt(t.Context(), openresponses.UserText(text)); err != nil {
					t.Fatal(err)
				}
			}
			switchModel(t, kit, agent, "small", openresponses.ReasoningEffortLow)
			if err := ctl.Compact(t.Context(), ""); err != nil {
				t.Fatal(err)
			}
			if _, err := ctl.Prompt(t.Context(), openresponses.UserText("three")); err != nil {
				t.Fatal(err)
			}
			writer := model
			if c.cheap {
				writer = cheap
				if n := len(summaries(model)); n != 0 {
					t.Errorf("the agent's model was asked %d summaries", n)
				}
			}
			sum := summaries(writer)
			if len(sum) != 1 {
				t.Fatalf("summary requests = %d, want 1", len(sum))
			}
			if sum[0].Model != c.want || sum[0].Reasoning.Effort != c.effort {
				t.Errorf("the summary was asked of %q at effort %q, want %q at %q", sum[0].Model, sum[0].Reasoning.Effort, c.want, c.effort)
			}
		})
	}
}
