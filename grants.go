package agentkit

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// SkillGrant is what happened when a skill the model read asked for
// tools. It is reported through the function [WithSkillGrantReport] was
// given, once per read, and once per live read [New] or
// [Kit.RegrantSkills] granted again, with Replayed set.
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
	// Err is set when nothing was granted because the skill's
	// allowed-tools would not parse, or because the read was made in a
	// conversation the kit's grants do not belong to,
	// [ErrSkillGrantConversation].
	Err error
	// Replayed is true for a grant made again from a session's records,
	// at [New] or by [Kit.RegrantSkills], rather than for a read the
	// model made just now.
	Replayed bool
}

// ErrSkillGrantConversation is the [SkillGrant.Err] of a read the kit
// would not grant because its grants belong to one conversation and the
// kit serves more than one. A grant is a rule set on the kit's one
// engine, which applies it to every decision the engine makes, so a
// grant made for one conversation would let every other conversation
// the kit serves run what the skill allows.
//
// The grants belong to the session the kit opened or was given, or, for
// a kit with none, to the conversation the first grant was made in,
// named by the session its run records into, [ContextWithRecorder]. The
// first call the kit decides in any other conversation revokes every
// grant, before that call is decided, and from then on the kit grants
// nothing: each read is reported with this error and the calls are
// decided by the policy alone. A front that grants skills to many
// conversations gives each its own kit. Runs recorded nowhere are one
// conversation to the kit, which cannot tell them apart.
var ErrSkillGrantConversation = errors.New("agentkit: this kit's skill grants belong to another conversation")

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
	// so the session's journal says every grant ended, and a read refused
	// in another conversation. It is nil under WithEngine, where the
	// kit's observer is not the engine's and nothing the engine grants
	// is recorded by the kit.
	observe func(context.Context, agentpolicy.Verdict)
	// conv names the conversation a run on the context belongs to: the
	// session its recorder writes, or "" for a run recorded nowhere.
	conv func(context.Context) string
	// scoped is WithSkillGrantScope.
	scoped bool

	// gmu is held from the test that a conversation may be granted to the
	// end of its GrantSet, and across the revocation when a second
	// conversation is seen, so no grant lands after that revocation.
	gmu sync.Mutex

	// mu guards sources, the names of every source a read has granted
	// under, which is what [Kit.RevokeSkillGrants] revokes; owner, the
	// conversation the grants belong to once bound is set, and ownerRec,
	// the recorder of its first grant, which a revocation is written to;
	// and shared, set once the kit has decided a call in another
	// conversation. A name stays once granted: revoking one the engine no
	// longer holds is a no-op.
	mu       sync.Mutex
	sources  map[string]bool
	owner    string
	ownerRec *session.Recorder
	bound    bool
	shared   bool
}

// claim reports whether grants may be made for the conversation of a run
// on ctx, binding the grants to it when they belong to none yet. The
// caller holds gmu.
func (g *skillGrants) claim(ctx context.Context) (string, bool) {
	conv := g.conv(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.bound {
		g.owner, g.ownerRec, g.bound = conv, RecorderFromContext(ctx), true
	}
	return conv, !g.shared && g.owner == conv
}

// owns reports whether the grants belong to the conversation of a run
// on ctx and the kit still grants.
func (g *skillGrants) owns(ctx context.Context) bool {
	conv := g.conv(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.bound && !g.shared && g.owner == conv
}

// guard is the BeforeToolCall hook the kit puts ahead of the engine under
// WithSkillGrants, and decides nothing: the first call it sees in a
// conversation the grants do not belong to revokes every grant, in the
// owner's session, before the engine decides that call, and stops the
// kit granting. [ErrSkillGrantConversation] says why.
func (g *skillGrants) guard(ctx context.Context, _ agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	conv := g.conv(ctx)
	g.mu.Lock()
	trip := g.bound && !g.shared && g.owner != conv
	g.mu.Unlock()
	if !trip {
		return nil, nil
	}
	g.gmu.Lock()
	defer g.gmu.Unlock()
	g.mu.Lock()
	if g.shared {
		g.mu.Unlock()
		return nil, nil
	}
	g.shared = true
	rec := g.ownerRec
	g.mu.Unlock()
	// Written where the grants were made: the owner's run is not this
	// one, so the context carries the owner's recorder and nothing of the
	// run that tripped it.
	g.revoke(ContextWithRecorder(context.WithoutCancel(context.Background()), rec))
	return nil, nil
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
		g.gmu.Lock()
		defer g.gmu.Unlock()
		if conv, ok := g.claim(ctx); !ok {
			g.refuse(ctx, read.Name, conv)
			return res, nil
		}
		g.grant(ctx, read.Name, nil)
		return res, nil
	})
}

// refuse reports, and records in the reading conversation's session, a
// read whose grant the kit will not make because its grants belong to
// another conversation. The model still gets the skill's text; its
// calls are decided by the policy alone.
func (g *skillGrants) refuse(ctx context.Context, name, conv string) {
	sk, ok := g.cat.Lookup(name)
	if !ok {
		return
	}
	g.mu.Lock()
	owner := g.owner
	g.mu.Unlock()
	err := fmt.Errorf("%w: skill %s was read in session %q, the grants belong to %q, and the kit serves more than one conversation", ErrSkillGrantConversation, sk.ListedName(), conv, owner)
	if g.observe != nil {
		g.observe(ctx, agentpolicy.Verdict{Tool: agentskill.ToolName, Action: agentturn.Block, Reason: "not granted the tools of skill " + sk.ListedName() + ": the kit serves more than one conversation and its skill grants belong to one", By: agentpolicy.ByPolicy})
	}
	g.tell(SkillGrant{Skill: sk.ListedName(), Location: sk.Location, Err: err})
}

// grant grants the skill's rules. Every read grants and reports: a
// repeated GrantSet under one source name replaces the set, so a read
// after a revoke puts the grant back and a read before one costs one
// engine call.
//
// only, when not nil, is a replay's: the rules the session recorded as
// granted for the read, by their text, and only the skill's rules among
// them are granted again, silently, under a context the kit's observer
// passes over.
func (g *skillGrants) grant(ctx context.Context, name string, only map[string]bool) {
	sk, ok := g.cat.Lookup(name)
	if !ok {
		return
	}
	out := SkillGrant{Skill: sk.ListedName(), Location: sk.Location, Replayed: only != nil}
	rules, err := sk.Rules()
	if err != nil {
		out.Err = fmt.Errorf("agentkit: skill %s: allowed-tools: %w", sk.ListedName(), err)
		g.tell(out)
		return
	}
	set := agentpolicy.RuleSet{Source: g.sourceOf(sk)}
	for _, r := range rules {
		rule := agentpolicy.Rule{Tool: r.Tool, Spec: r.Spec, Source: set.Source}
		if only != nil && !only[rule.String()] {
			continue
		}
		set.Allow = append(set.Allow, rule)
	}
	if len(set.Allow) == 0 {
		return
	}
	if only != nil {
		ctx = context.WithValue(ctx, replayKey{}, true)
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

// regrant grants again, at New or from [Kit.RegrantSkills], what the
// skill reads on the session's path granted and nothing had revoked when
// the kit that made them stopped: a grant lives in the engine, and a
// restart between a held call and its approval lost it, so the
// approval's Resume went on without the tools the skill had been
// granted. The grants then belong to that session.
//
// The path is replayed forward, so the journal decides. Each
// agentskill.Read record of a skill's own instructions makes its source
// live, and each verdict the engine recorded when it revoked a source,
// [revokedPrefix], ends it, whether WithSkillGrantScope, the product's
// Kit.RevokeSkillGrants or anything else revoked it; the kit records
// the revocation of a set that held no rules itself, since the engine
// does not. A record whose name the catalogue now gives a skill at
// another location is passed over, and so is one whose digest is not
// what the catalogue's tool serves for the name now: that is not the
// skill the model read.
//
// A restart may narrow a grant and never widen it. When the kit built
// the engine, and so recorded its verdicts, what is granted again is
// the rules the path says the engine granted for the read, the
// "granted <rule> by <source>" verdicts written just before its record,
// that the catalogue still gives the skill: a skill whose allowed-tools
// were widened, or whose source turned trusted, since the read gets
// nothing it did not have. Under WithEngine the kit records nothing, and
// what is granted is the catalogue's rules as they stand now.
//
// The replay is silent: no verdict is recorded for it, since the ones it
// repeats are still on the path, and the report is made with
// [SkillGrant.Replayed] set.
//
// Under the scope the replay also starts after the path's last user
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
	type live struct {
		name, source string
		rules        map[string]bool
	}
	var granted []live
	// pending is the rules each source was granted since its last read
	// record or revocation, which the next read record of it takes.
	pending := map[string]map[string]bool{}
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
			rules := pending[source]
			delete(pending, source)
			if g.servedSHA(ctx, read.Name) != read.SHA256 {
				continue
			}
			if g.observe != nil && rules == nil {
				rules = map[string]bool{}
			}
			granted = append(granted, live{name: read.Name, source: source, rules: rules})
		case agentpolicy.VerdictNS:
			var v struct {
				Action string `json:"action"`
				Rule   string `json:"rule"`
				Source string `json:"source"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal(c.Data, &v) != nil {
				continue
			}
			switch {
			case strings.HasPrefix(v.Reason, revokedPrefix):
				source := strings.TrimPrefix(v.Reason, revokedPrefix)
				drop(source)
				delete(pending, source)
			case v.Action == "allow" && v.Rule != "" && strings.HasPrefix(v.Reason, "granted "):
				if pending[v.Source] == nil {
					pending[v.Source] = map[string]bool{}
				}
				pending[v.Source][v.Rule] = true
			}
		}
	}
	for _, l := range granted {
		only := l.rules
		if only == nil {
			// Under WithEngine: the catalogue's rules, every one of them.
			only = map[string]bool{}
			if sk, ok := g.cat.Lookup(l.name); ok {
				if rules, err := sk.Rules(); err == nil {
					for _, r := range rules {
						only[agentpolicy.Rule{Tool: r.Tool, Spec: r.Spec}.String()] = true
					}
				}
			}
		}
		g.grant(ctx, l.name, only)
	}
}

// servedSHA is the digest the catalogue's tool serves for a read of the
// skill's instructions now, "" when it serves none.
func (g *skillGrants) servedSHA(ctx context.Context, name string) string {
	args, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return ""
	}
	res, err := g.cat.Tool().Execute(ctx, agenttool.Call{Args: args})
	if err != nil {
		return ""
	}
	read, _ := res.Details.(agentskill.Read)
	return read.SHA256
}

// replayKey marks the context of a replay's GrantSet, whose verdicts the
// kit's observer neither records nor hands on.
type replayKey struct{}

func replaying(ctx context.Context) bool {
	v, _ := ctx.Value(replayKey{}).(bool)
	return v
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
