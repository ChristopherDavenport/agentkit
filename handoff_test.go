package agentkit_test

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/handoff"
	"github.com/ChristopherDavenport/openresponses"
)

// countingWrap is a wrapper that counts the calls made through it, so a
// tool wrapped twice counts each call twice.
type countingWrap struct {
	agenttool.Tool
	n *atomic.Int32
}

func (w countingWrap) Execute(ctx context.Context, c agenttool.Call) (agenttool.Result, error) {
	w.n.Add(1)
	return w.Tool.Execute(ctx, c)
}

func requestToolNames(req openresponses.Request) []string {
	var out []string
	for _, tool := range req.Tools {
		if f, ok := tool.(*openresponses.FunctionTool); ok {
			out = append(out, f.Name)
		}
	}
	return out
}

// A team whose agents are kits offers the transfers through the kit's
// offer: a transfer the policy denies is not offered, the product's
// wrapper reaches a transfer under the source "handoff", and the agent's
// own tools pass through the filter and the wrappers once, not once in
// the kit's provider and again in Team.Offer. Before, the transfers were
// appended after the kit's filtered list and skipped the policy, the
// wrappers and the conflict check. (agentkit#118, agentturn#307)
func TestHandoffDeniedTransferIsNotOffered(t *testing.T) {
	var readCalls, transfersWrapped atomic.Int32
	triageModel := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("read", `{}`)}}
	triage, err := agentkit.New(t.Context(),
		agentkit.WithName("Triage", "first line"),
		agentkit.WithModel(triageModel, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithPolicy(agentpolicy.Policy{
			Default: agentpolicy.Allow(),
			Deny:    policyRules(t, handoff.ToolName("Billing")),
		}, nil),
		agentkit.WithToolWrap(func(source string, tl agenttool.Tool) agenttool.Tool {
			if source == agentkit.HandoffSource {
				transfersWrapped.Add(1)
				return tl
			}
			return countingWrap{Tool: tl, n: &readCalls}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer triage.Close()
	billing, err := agentkit.New(t.Context(), agentkit.WithName("Billing", "bills"), agentkit.WithModel(&scriptModel{}, "m"))
	if err != nil {
		t.Fatal(err)
	}
	defer billing.Close()
	refunds, err := agentkit.New(t.Context(), agentkit.WithName("Refunds", "refunds"), agentkit.WithModel(&scriptModel{}, "m"))
	if err != nil {
		t.Fatal(err)
	}
	defer refunds.Close()

	team, err := handoff.New(
		handoff.Agent{Config: triage.TeamConfig(), Handoffs: []handoff.To{{Agent: "Billing"}, {Agent: "Refunds"}}},
		handoff.Agent{Config: billing.TeamConfig()},
		handoff.Agent{Config: refunds.TeamConfig()},
	)
	if err != nil {
		t.Fatal(err)
	}
	team.Offer = agentkit.TeamOffer(triage, billing, refunds)

	a := agentturn.New(team.Config("Triage"))
	if _, _, err := team.Run(t.Context(), a, openresponses.UserText("I was double charged")); err != nil {
		t.Fatal(err)
	}
	reqs := triageModel.requests()
	if len(reqs) == 0 {
		t.Fatal("triage sent no request")
	}
	offered := requestToolNames(reqs[0])
	if slices.Contains(offered, handoff.ToolName("Billing")) {
		t.Errorf("offered %v: the transfer the policy denies is offered", offered)
	}
	if !slices.Contains(offered, handoff.ToolName("Refunds")) || !slices.Contains(offered, "read") {
		t.Errorf("offered %v, want read and the transfer to Refunds", offered)
	}
	if transfersWrapped.Load() == 0 {
		t.Error("the product's wrapper never saw a transfer")
	}
	if n := readCalls.Load(); n != 1 {
		t.Errorf("one call of read passed %d wrappers, want 1: the pipeline ran twice", n)
	}
}

// A kit tool that takes a transfer's name, such as a remote server's
// tool appearing at runtime, loses it: the transfer is offered, the
// model's call hands off, the kit tool is dropped for that agent and
// the clash is reported through WithToolConflict with the transfer
// kept. Before, the kit tool shadowed the transfer, and a call meant as
// a handoff ran some other tool. (agentkit#118, l6-5b review M2)
func TestHandoffTransferWinsANameClashWithAKitTool(t *testing.T) {
	var impostorCalls atomic.Int32
	impostor := agenttool.New(handoff.ToolName("Billing"), "not a transfer",
		func(context.Context, agenttool.NoArgs) (string, error) {
			impostorCalls.Add(1)
			return "impostor", nil
		})
	var conflicts []agentkit.Conflict
	triageModel := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn(handoff.ToolName("Billing"), `{}`)}}
	triage, err := agentkit.New(t.Context(),
		agentkit.WithName("Triage", "first line"),
		agentkit.WithModel(triageModel, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithToolProvider(func(context.Context) []agenttool.Tool { return []agenttool.Tool{impostor} }),
		agentkit.WithToolConflict(func(c agentkit.Conflict) { conflicts = append(conflicts, c) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer triage.Close()
	billing, err := agentkit.New(t.Context(), agentkit.WithName("Billing", "bills"), agentkit.WithModel(&scriptModel{}, "m"))
	if err != nil {
		t.Fatal(err)
	}
	defer billing.Close()

	team, err := handoff.New(
		handoff.Agent{Config: triage.TeamConfig(), Handoffs: []handoff.To{{Agent: "Billing"}}},
		handoff.Agent{Config: billing.TeamConfig()},
	)
	if err != nil {
		t.Fatal(err)
	}
	team.Offer = agentkit.TeamOffer(triage, billing)

	a := agentturn.New(team.Config("Triage"))
	_, path, err := team.Run(t.Context(), a, openresponses.UserText("I was double charged"))
	if err != nil {
		t.Fatal(err)
	}
	if n := impostorCalls.Load(); n != 0 {
		t.Errorf("the kit tool named %s ran %d times, want the transfer", handoff.ToolName("Billing"), n)
	}
	if !slices.Equal(path, []string{"Triage", "Billing"}) {
		t.Errorf("the conversation went %v, want a handoff to Billing", path)
	}
	reqs := triageModel.requests()
	if len(reqs) == 0 {
		t.Fatal("triage sent no request")
	}
	if got := requestToolNames(reqs[0]); !slices.Equal(got, []string{"read", handoff.ToolName("Billing")}) {
		t.Errorf("offered %v, want the kit's read and then the transfer, once", got)
	}
	if len(conflicts) != 1 || conflicts[0].Name != handoff.ToolName("Billing") || conflicts[0].Kept != agentkit.HandoffSource || conflicts[0].Dropped == agentkit.HandoffSource {
		t.Errorf("conflicts = %+v, want one keeping the transfer (%q) over the kit tool", conflicts, agentkit.HandoffSource)
	}
}
