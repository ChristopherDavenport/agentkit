package agentkit_test

import (
	"slices"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// Kit.GrantScope names the scope the kit's engine decides every call
// under, with skill grants or without: a set a front activates under it,
// a person's "for this session", decides the conversation's next calls.
// Without skills the kit decided unscoped, the set never fired, and the
// person was asked again for every call. (agentpolicy RFC 0001 Grant
// sets, #94)
func TestASetGrantedUnderTheKitsScopeDecidesWithoutSkills(t *testing.T) {
	var ran atomic.Int32
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("bash", `{}`), textTurn("done"),
		callTurn("bash", `{}`), textTurn("done"),
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithTools(counting("bash", &ran)),
		agentkit.WithPolicy(agentpolicy.Policy{Default: agentpolicy.Ask()}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	agent := agentturn.New(kit.Config())
	end, err := agent.Prompt(t.Context(), openresponses.UserText("run it"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 1 {
		t.Fatalf("first call: reason %q, pending %d; want it asked", end.Reason, len(end.Pending))
	}
	// The person answers "for this session": the front grants the rule
	// under the conversation's scope and approves the call.
	ctx := agentpolicy.ContextWithGrantScope(t.Context(), kit.GrantScope(t.Context()))
	if granted, refused := kit.Engine().GrantSet(ctx, agentpolicy.RuleSet{
		Source: agentpolicy.Source{Name: "session", Trusted: true},
		Allow:  policyRules(t, "bash"),
	}); len(granted) != 1 || len(refused) != 0 {
		t.Fatalf("GrantSet granted %v, refused %v", granted, refused)
	}
	if _, err := agent.Resume(t.Context(), agentturn.Approve(end.Pending[0].Call.CallID).WithBy(agentpolicy.ByHuman)); err != nil {
		t.Fatal(err)
	}
	end, err = agent.Prompt(t.Context(), openresponses.UserText("run it again"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason == agentturn.ReasonInputRequired || ran.Load() != 2 {
		t.Fatalf("second call: reason %q, bash ran %d times; want it allowed by the session's grant", end.Reason, ran.Load())
	}
}

// The offer reads the sets a decision under the same scope reads, with
// skill grants or without: a front's "never, for this session", a bare
// deny activated under Kit.GrantScope, takes the tool off what the
// conversation is offered, as it blocks the call. Without skills the
// offer was filtered unscoped, so the model kept being offered a tool
// every call of which was blocked. (agentpolicy RFC 0001 Grant sets, #94)
func TestASetDeniedUnderTheKitsScopeLeavesTheOfferWithoutSkills(t *testing.T) {
	var ran atomic.Int32
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "m"),
		agentkit.WithTools(counting("bash", &ran), counting("read", &ran)),
		agentkit.WithPolicy(agentpolicy.Policy{Default: agentpolicy.Allow()}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	ctx := agentpolicy.ContextWithGrantScope(t.Context(), kit.GrantScope(t.Context()))
	kit.Engine().GrantSet(ctx, agentpolicy.RuleSet{
		Source: agentpolicy.Source{Name: "session", Trusted: true},
		Deny:   policyRules(t, "bash"),
	})
	offered := toolNames(kit.Config().ResolveTools(t.Context()))
	if slices.Contains(offered, "bash") || !slices.Contains(offered, "read") {
		t.Fatalf("offered %v; want bash off the offer under the session's deny, read still offered", offered)
	}
}
