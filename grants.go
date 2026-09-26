package agentkit

import (
	"context"
	"fmt"
	"sync"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
)

// SkillGrant is what happened when a skill the model read asked for
// tools. It is reported through the function [WithSkillGrants] was
// given, when that function is not nil.
type SkillGrant struct {
	// Skill is the skill that was read.
	Skill string
	// Location is the SKILL.md behind the name.
	Location string
	// Granted are the rules the engine took.
	Granted []agentpolicy.Rule
	// Refused are the rules it would not take, each with the engine's
	// reason.
	Refused []agentpolicy.Refusal
	// Err is set when the skill's allowed-tools would not parse, in
	// which case nothing was granted.
	Err error
}

// grantingTool wraps the skill catalogue's tool so that reading a skill
// grants that skill's allowed-tools to the policy engine.
//
// This is composition no library can do: agentskill owns the grammar
// and refuses to widen it, agentpolicy owns the same grammar and the
// engine, and neither imports the other. The kit is where the two meet,
// and the whole of the meeting is [agentskill.Skill.Rules] to
// [agentpolicy.Engine.GrantSet] with a [agentpolicy.Source] the product
// chose.
type grantingTool struct {
	agenttool.Tool

	cat    *agentskill.Catalog
	engine *agentpolicy.Engine
	source func(*agentskill.Skill) agentpolicy.Source
	report func(SkillGrant)

	// mu guards granted, so a skill read twice is granted once and the
	// product is told once.
	mu      sync.Mutex
	granted map[string]bool
}

func (g *grantingTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
	res, err := g.Tool.Execute(ctx, call)
	if err != nil {
		return res, err
	}
	read, ok := res.Details.(agentskill.Read)
	if !ok || read.Path != "" {
		// A read of a file inside a skill, or a result whose details
		// this version of agentskill does not set. The grant belongs to
		// the read of the skill's own instructions, which is the call
		// that tells the model what to do.
		return res, nil
	}
	g.grant(ctx, read.Name)
	return res, nil
}

func (g *grantingTool) grant(ctx context.Context, name string) {
	sk, ok := g.cat.Lookup(name)
	if !ok {
		return
	}
	g.mu.Lock()
	if g.granted[sk.Location] {
		g.mu.Unlock()
		return
	}
	if g.granted == nil {
		g.granted = map[string]bool{}
	}
	g.granted[sk.Location] = true
	g.mu.Unlock()

	out := SkillGrant{Skill: sk.Name, Location: sk.Location}
	rules, err := sk.Rules()
	if err != nil {
		out.Err = fmt.Errorf("agentkit: skill %s: allowed-tools: %w", sk.Name, err)
		g.tell(out)
		return
	}
	if len(rules) == 0 {
		return
	}
	set := agentpolicy.RuleSet{Source: g.sourceOf(sk)}
	for _, r := range rules {
		set.Allow = append(set.Allow, agentpolicy.Rule{Tool: r.Tool, Spec: r.Spec, Source: set.Source})
	}
	out.Granted, out.Refused = g.engine.GrantSet(ctx, set)
	g.tell(out)
}

func (g *grantingTool) sourceOf(sk *agentskill.Skill) agentpolicy.Source {
	if g.source != nil {
		return g.source(sk)
	}
	// Untrusted, so the allow rules are withheld. A skill is a file
	// someone else wrote; a product that trusts the tree it came from
	// says so in its own source function.
	return agentpolicy.Source{Name: "agentskill:" + sk.Name, Path: sk.Location}
}

func (g *grantingTool) tell(s SkillGrant) {
	if g.report != nil {
		g.report(s)
	}
}

// The embedded Tool supplies Name, Description and Parameters
// unchanged, so the model is offered exactly the tool agentskill built.
//
// Embedding an interface forwards only that interface's methods, so an
// optional one the catalogue's tool grew would be lost here silently.
// That is a known agenttool limitation, not a fault of this type:
// every middleware over a Tool drops the same five — Sequential,
// Resource, Annotated, Strict and Confined. (Schemer is on the
// argument type rather than the Tool, so no Tool wrapper can forward
// it. Recordable is on the Details value, which Execute returns
// untouched.) Until a forwarding helper lands upstream,
// TestTheGrantingWrapperIsTheSameToolToTheLoop compares all five
// against the unwrapped tool, so the day agentskill implements one
// this fails rather than quietly changing how the batch runs.
//
// This wrapper is also the one place the package's rule bends, since a
// product cannot construct it. Both halves have the same fix and it is
// upstream: an exported wrapper in agenttool forwards what embedding
// drops, and, being exported, is a call a product could write, so the
// manual path stops needing a type only the kit has. This type and its
// test go when that lands.
var _ agenttool.Tool = (*grantingTool)(nil)

// skillRuleProblems reports the skills whose allowed-tools will not
// parse. They are found at New rather than at the first read, so a
// typo in a skill is an omission the product can print before the run
// instead of a surprise in the middle of one.
func skillRuleProblems(cat *agentskill.Catalog) []Omission {
	var out []Omission
	for _, sk := range cat.Listed() {
		if _, err := sk.Rules(); err != nil {
			out = append(out, Omission{
				Part:   PartSkills,
				Source: SourceSkills,
				What:   skillKey(sk),
				Reason: "allowed-tools will not parse: " + err.Error(),
			})
		}
	}
	return out
}
