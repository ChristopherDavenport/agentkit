package agentkit

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// SkillGrant is what happened when a skill the model read asked for
// tools. It is reported through the function [WithSkillGrantReport] was
// given, once per read.
type SkillGrant struct {
	// Skill is the skill that was read, by the name the catalogue lists
	// it under, [agentskill.Skill.ListedName]: "deploy" for a root
	// skill and "apps/web:deploy" for a qualified one that shares its
	// name, so the two are two grants and two reports.
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

// skillGrants is the meeting of a skill's allowed-tools and the policy
// engine: reading a skill grants that skill's rules.
//
// This is composition no library can do: agentskill owns the grammar
// and refuses to widen it, agentpolicy owns the same grammar and the
// engine, and neither imports the other. The kit is where the two meet,
// and the whole of the meeting is [agentskill.Skill.Rules] to
// [agentpolicy.Engine.GrantSet] with a [agentpolicy.Source] the product
// chose, around the catalogue's tool through [agenttool.Wrap], which
// keeps every property the tool declares.
type skillGrants struct {
	cat    *agentskill.Catalog
	engine *agentpolicy.Engine
	source func(*agentskill.Skill) agentpolicy.Source
	report func(SkillGrant)
	// observe, when the kit built the engine, records a revocation of a
	// source whose set held no rules, which the engine does not report,
	// so the session's journal says every grant ended.
	observe func(context.Context, agentpolicy.Verdict)

	// mu guards sources, the names of every source a read has granted
	// under, which is what [Kit.RevokeSkillGrants] revokes. A name stays
	// once granted: revoking one the engine no longer holds is a no-op.
	mu      sync.Mutex
	sources map[string]bool
}

// wrap returns the catalogue's tool, or whatever stands in for it,
// granting on each read of a skill's own instructions.
func (g *skillGrants) wrap(t agenttool.Tool) agenttool.Tool {
	return agenttool.Wrap(t, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
		res, err := t.Execute(ctx, call)
		if err != nil {
			return res, err
		}
		read, ok := res.Details.(agentskill.Read)
		if !ok || read.Path != "" {
			// A read of a file inside a skill, or a result whose details
			// this version of agentskill does not set. The grant belongs
			// to the read of the skill's own instructions, which is the
			// call that tells the model what to do.
			return res, nil
		}
		g.grant(ctx, read.Name)
		return res, nil
	})
}

// grant grants the skill's rules. Every read grants and reports: a
// repeated GrantSet under one source name replaces the set, so a read
// after a revoke puts the grant back and a read before one costs one
// engine call.
func (g *skillGrants) grant(ctx context.Context, name string) {
	sk, ok := g.cat.Lookup(name)
	if !ok {
		return
	}
	out := SkillGrant{Skill: sk.ListedName(), Location: sk.Location}
	rules, err := sk.Rules()
	if err != nil {
		out.Err = fmt.Errorf("agentkit: skill %s: allowed-tools: %w", sk.ListedName(), err)
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
	g.mu.Lock()
	if g.sources == nil {
		g.sources = map[string]bool{}
	}
	g.sources[set.Source.Name] = true
	g.mu.Unlock()
	out.Granted, out.Refused = g.engine.GrantSet(ctx, set)
	g.tell(out)
}

// regrant grants again, at New, what the skill reads on the session's
// path granted and nothing had revoked when the kit that made them
// stopped: a grant lives in the engine, and a restart between a held
// call and its approval lost it, so the approval's Resume went on
// without the tools the skill had been granted.
//
// The path is replayed forward, so the journal decides. Each
// agentskill.Read record of a skill's own instructions grants its
// source, and each verdict the engine recorded when it revoked a source,
// [revokedPrefix], ends it, whether WithSkillGrantScope, the product's
// Kit.RevokeSkillGrants or anything else revoked it; the kit records
// the revocation of a set that held no rules itself, since the engine
// does not. A record whose name the catalogue now gives a skill at
// another location is passed over: that is not the skill the model
// read. What is granted is the catalogue's rules as they stand now.
//
// Under scoped the replay also starts after the path's last user
// message, which revoked everything before it, so the scope holds even
// where the journal is silent: an engine the product built, whose
// revocations reach the session only if the product records them, or a
// verdict whose write failed. The journal matches a revocation to a
// read by the source name the product's source function gives now; a
// function that renames its sources between releases loses the match.
func (g *skillGrants) regrant(ctx context.Context, sess *agentsession.Session, scoped bool) {
	path := sess.Path(sess.Leaf())
	from := 0
	if scoped {
		for i := len(path) - 1; i >= 0; i-- {
			if e, ok := path[i].(*agentsession.ItemEntry); ok {
				item, _ := agentturn.Unhide(e.Item)
				if m, ok := item.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
					from = i + 1
					break
				}
			}
		}
	}
	type live struct{ name, source string }
	var granted []live
	drop := func(source string) {
		granted = slices.DeleteFunc(granted, func(l live) bool { return l.source == source })
	}
	for _, e := range path[from:] {
		c, ok := e.(*agentsession.CustomEntry)
		if !ok {
			continue
		}
		switch c.NS {
		case agentskill.RecordNS:
			var read agentskill.Read
			if json.Unmarshal(c.Data, &read) != nil || read.Path != "" {
				continue
			}
			sk, ok := g.cat.Lookup(read.Name)
			if !ok || sk.Location != read.Location {
				continue
			}
			source := g.sourceOf(sk).Name
			drop(source)
			granted = append(granted, live{name: read.Name, source: source})
		case agentpolicy.VerdictNS:
			var v struct{ Reason string }
			if json.Unmarshal(c.Data, &v) == nil && strings.HasPrefix(v.Reason, revokedPrefix) {
				drop(strings.TrimPrefix(v.Reason, revokedPrefix))
			}
		}
	}
	for _, l := range granted {
		g.grant(ctx, l.name)
	}
}

// revokedPrefix opens the reason of the verdict agentpolicy's
// Engine.Revoke records, which names the source after it.
const revokedPrefix = "revoked the rules granted by "

// revoke revokes every source a read granted under and returns the
// number of rules the engine removed.
func (g *skillGrants) revoke(ctx context.Context) int {
	g.mu.Lock()
	names := make([]string, 0, len(g.sources))
	for name := range g.sources {
		names = append(names, name)
	}
	g.mu.Unlock()
	sort.Strings(names)
	n := 0
	for _, name := range names {
		removed := g.engine.Revoke(ctx, name)
		if removed == 0 && g.observe != nil {
			// The engine records a revocation only when it removed a rule.
			// A skill's set held none, untrusted or refused, and a restart
			// that replays the journal must still see it end, or a rule the
			// skill gains by then is granted for a read the scope ended.
			g.observe(ctx, agentpolicy.Verdict{Action: agentturn.Block, Reason: revokedPrefix + name, By: agentpolicy.ByPolicy})
		}
		n += removed
	}
	return n
}

func (g *skillGrants) sourceOf(sk *agentskill.Skill) agentpolicy.Source {
	if g.source != nil {
		return g.source(sk)
	}
	// Untrusted, so the allow rules are withheld. A skill is a file
	// someone else wrote; a product that trusts the tree it came from
	// says so in its own source function. The listed name, not Name: a
	// root deploy and a qualified apps/web:deploy share Name, and the
	// engine keys a grant set by its source's name, so reading one
	// would replace the other's.
	return agentpolicy.Source{Name: "agentskill:" + sk.ListedName(), Path: sk.Location}
}

func (g *skillGrants) tell(s SkillGrant) {
	if g.report != nil {
		g.report(s)
	}
}

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
