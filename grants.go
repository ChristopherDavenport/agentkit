package agentkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// Err is set when nothing was granted: because the skill's
	// allowed-tools would not parse; because the catalogue's tool
	// refused the read, the skill file gone, renamed or unparseable
	// since discovery, [agentskill.ErrSkillChanged], a report a front
	// reloads on with [Kit.ReloadSkills]; or, with Replayed set, because
	// a restart would not grant a read again: the skill changed since
	// the read, [ErrSkillGrantChanged], the session records no verdict
	// of the read's grant, [ErrSkillGrantUnrecorded], or none of the
	// rules the session recorded for the read is among the skill's now.
	Err error
	// Scope is the grant scope the read was made in, the conversation
	// whose calls the grant decides, [Kit.GrantScope]. A kit that serves
	// many conversations reports every one's reads to one function, and
	// this says whose.
	Scope string
	// Replayed is true for a grant made again from a session's records,
	// at [New] or by [Kit.RegrantSkills], rather than for a read the
	// model made just now.
	Replayed bool
	// FrontmatterChanged is the read's [agentskill.Read]
	// FrontmatterChanged: the skill file on disk has other frontmatter
	// than the catalogue was built from, so the grant is the rules as
	// loaded, and [Kit.ReloadSkills] brings in the new ones.
	FrontmatterChanged bool
}

// ErrSkillGrantChanged is the [SkillGrant.Err] of a read a restart will
// not grant again because the skill is not what the model read: the
// digest of the instructions the catalogue's tool serves for it now, or
// of its frontmatter, is not the one the read recorded. The report has
// [SkillGrant.Replayed] set and [SkillGrant.FrontmatterChanged] says
// which digest moved; the session records a verdict saying the same.
// Nothing is granted: a read of the skill grants by the skill as it is
// now, and a front that shows the user the policy in force shows that
// the skill changed since they approved it.
var ErrSkillGrantChanged = errors.New("agentkit: the skill changed since it was read, so its grant was not made again")

// ErrSkillGrantUnrecorded is the [SkillGrant.Err] of a live read, found
// by [New] or [Kit.RegrantSkills] in a session a kit that records its
// engine's verdicts is to grant again, for which the session records no
// verdict of the grant: the engine observes a verdict for every rule a
// GrantSet grants or refuses, so none means the kit's observer wrote
// nowhere when the read was made, as it does when the run's context
// carries no recorder, [ContextWithRecorder], and the kit has none.
// Nothing is granted: a restart grants what the session says the read
// was granted, and this session says nothing.
var ErrSkillGrantUnrecorded = errors.New("agentkit: the session records no grant for the read, so it was not made again")

// ErrSkillGrantRecorder is the error [Kit.RegrantSkills] returns, before
// it binds or grants anything, when the kit records its engine's
// verdicts and no recorder writes the session it was given: neither the
// one on ctx, [ContextWithRecorder], nor the kit's own. The grants'
// verdicts and a later revocation would be recorded nowhere, and the
// next restart would find a session that says nothing about them.
var ErrSkillGrantRecorder = errors.New("agentkit: no recorder writes the session, so its skill grants would be recorded nowhere")

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
//
// Each grant belongs to the conversation that read the skill: the engine
// keeps it under the grant scope [Kit.GrantScope] names for the run,
// [agentpolicy.ContextWithGrantScope], and decides only the calls made
// under that scope. The kit's one engine serves every conversation, and
// no conversation's grant reaches another's.
type skillGrants struct {
	// cat is the catalogue as it stands, which [Kit.ReloadSkills] may
	// replace.
	cat    func() *agentskill.Catalog
	engine *agentpolicy.Engine
	source func(*agentskill.Skill) agentpolicy.Source
	report func(SkillGrant)
	// observe, when the kit built the engine, records a revocation of a
	// source whose set held no rules, which the engine does not report,
	// so the session's journal says every grant ended. It is nil under
	// WithEngine, where the kit's observer is not the engine's and
	// nothing the engine grants is recorded by the kit.
	observe func(context.Context, agentpolicy.Verdict)
	// scope names the grant scope a run on the context belongs to,
	// Kit.GrantScope.
	scope func(context.Context) string
	// mark is the user message a grant made under the context is bound
	// to, Kit.markFor, and false for a grant bound to none.
	mark func(context.Context) (userMark, bool)
	// matchers are WithPolicy's, which is how blockEnded reads a call's
	// subjects and matches an ended rule's specifier against them.
	matchers map[string]agentpolicy.ToolMatcher
	// scoped is WithSkillGrantScope.
	scoped bool
	// hooks is set when the engine was given hooks of the product's, the
	// WithBeforeToolCall ones or agentpolicy options that may add some.
	hooks bool

	// gmu is held from the start of a grant to the end of its GrantSet,
	// and across a revocation, so no grant lands in the engine after the
	// revocation that was to end it and goes unrecorded.
	// It is one lock for every conversation, so a slow journal write in
	// one delays a skill read in another; a lock per scope would remove
	// that.
	gmu sync.Mutex

	// mu guards scopes, the state of each grant scope a read has granted
	// in.
	mu     sync.Mutex
	scopes map[string]*scopeGrants
	// children is the grant scopes of the child agents run under each
	// scope, recorded when a child's read grants under its own scope, so
	// a conversation's message ends exactly those and not a scope whose
	// name merely begins with its own.
	children map[string]map[string]bool
}

// parentScopeKey carries, on the context of a child agent's run, the
// grant scope its parent runs under, [Kit.childContext].
type parentScopeKey struct{}

// link records scope as a child scope of parent. The caller holds mu.
func (g *skillGrants) link(parent, scope string) {
	if g.children == nil {
		g.children = map[string]map[string]bool{}
	}
	if g.children[parent] == nil {
		g.children[parent] = map[string]bool{}
	}
	g.children[parent][scope] = true
}

// descendants returns every scope run under scope, children and their
// children, in no order. The caller holds mu.
func (g *skillGrants) descendants(scope string) []string {
	var out []string
	seen := map[string]bool{scope: true}
	queue := []string{scope}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for child := range g.children[next] {
			if !seen[child] {
				seen[child] = true
				out = append(out, child)
				queue = append(queue, child)
			}
		}
	}
	return out
}

// scopeGrants is what the kit keeps of the grants of one grant scope:
// sources, the names of every source a read has granted under, which is
// what a revocation revokes; live, the grants in force, by source, which
// a revocation clears, for the note the scope gives the model; and
// ended, the grants the scope's last revocations ended, by source,
// newest kept, which blockEnded, the hook the kit folds into the
// engine, refuses a call by until the skill is read again. A name stays
// in sources once granted: revoking one the engine no longer holds is a
// no-op.
type scopeGrants struct {
	sources map[string]bool
	live    map[string]liveGrant
	ended   map[string]liveGrant
}

// liveGrant is a grant in force: the skill's listed name, the source
// the grant was made under, the rules the engine took and, when bound
// is set, the user message it was made under, which the scope ends it
// after.
type liveGrant struct {
	skill  string
	source string
	rules  []agentpolicy.Rule
	under  userMark
	bound  bool
}

// state returns the state of scope, making it. The caller holds mu.
func (g *skillGrants) state(scope string) *scopeGrants {
	if g.scopes == nil {
		g.scopes = map[string]*scopeGrants{}
	}
	st := g.scopes[scope]
	if st == nil {
		st = &scopeGrants{}
		g.scopes[scope] = st
	}
	return st
}

// scopedCtx returns ctx carrying the grant scope a run on it belongs to,
// which is what the engine's calls read.
func (g *skillGrants) scopedCtx(ctx context.Context) context.Context {
	return agentpolicy.ContextWithGrantScope(ctx, g.scope(ctx))
}

// stale reports whether a live grant in the grant scope of ctx, or in a
// child agent's under it, was made under a user message earlier than the
// one a transcript with mark now is under. A child's grant is bound to
// its parent's mark, [Kit.markFor], so a message in the conversation is
// one that ends it.
func (g *skillGrants) stale(ctx context.Context, now userMark) bool {
	scope := g.scope(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, sc := range append([]string{scope}, g.descendants(scope)...) {
		st := g.scopes[sc]
		if st == nil {
			continue
		}
		for _, lg := range st.live {
			if lg.bound && lg.under.stale(now) {
				return true
			}
		}
	}
	return false
}

// keepEnded remembers the grants the scope ended, by the source each
// was made under, for blockEnded.
func (g *skillGrants) keepEnded(scope string, ended []liveGrant) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(scope)
	if st.ended == nil {
		st.ended = map[string]liveGrant{}
	}
	for _, lg := range ended {
		st.ended[lg.source] = lg
	}
}

// clearEnded forgets the ended grants of the skill listed as name in a
// grant scope, on a read of it there.
func (g *skillGrants) clearEnded(scope, name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.scopes[scope]
	if st == nil {
		return
	}
	for source, lg := range st.ended {
		if lg.skill == name {
			delete(st.ended, source)
		}
	}
}

// pruneEnded forgets the ended grants, in every grant scope, of skills
// the catalogue no longer lists, after Kit.ReloadSkills: a skill deleted
// or renamed can be read again under no name, so its ended grant would
// refuse its tool for the rest of the conversation.
func (g *skillGrants) pruneEnded() {
	cat := g.cat()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, st := range g.scopes {
		for source, lg := range st.ended {
			if _, ok := cat.Lookup(lg.skill); !ok {
				delete(st.ended, source)
			}
		}
	}
}

// wouldKey marks the context of blockEnded's own evaluation, which folds
// the same hooks, blockEnded among them, as the decision it asks about.
type wouldKey struct{}

// blockEnded is the scope's refusal, the first hook the kit folds into
// the engine it builds, [agentpolicy.WithHooks], under WithSkillGrantScope:
// a call that every rule of a grant the scope ended would have allowed,
// and that the engine would only ask about, is blocked with a reason the
// model reads, naming the skill and the tool that reads it again. The
// engine would defer the call to the ask rule, and what the model read
// then was a reviewer's refusal with no word of the skill; the
// turn-start note is several items up and a model refused acts on the
// refusal in front of it. Inside the engine rather than chained ahead of
// it, so the engine's fold takes the Block: a sibling in the batch is
// decided beside a blocked call, not held for a question nobody is
// asked, and the engine records the decision as a verdict, so the kit
// writes none itself.
//
// A call is allowed by an ended grant when each of its subjects, the
// tool's [agentpolicy.Subjects] split of it under WithPolicy's matchers
// or the call itself, is covered by a rule of an ended grant of the
// grant scope the call is decided under: the rule names the subject's
// tool and is bare or its specifier matches the subject under the tool's
// matcher. An ended grant of a skill the catalogue no longer lists
// covers nothing.
//
// What the engine would do without those grants is [agentpolicy.Engine.Would]'s
// answer, the decision before the batch hold with the same rules, the
// grants still in force, the call's confinement and the hooks folded in,
// the product's among them, which are called once more for it. The kit
// refuses when the engine would ask, which is what the grant answered,
// unless the product gave the engine hooks and no ask rule is behind the
// verdict: it may be a hook's, which a grant would not have changed. A
// call the engine would allow is left to it, since an allow needs no
// grant. One it would deny is too: the engine folds its hooks in after a
// policy that denies, so this hook is not asked, and a deny rule beats
// a grant in any case.
func (g *skillGrants) blockEnded(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	if info.Call == nil || ctx.Value(wouldKey{}) != nil {
		return nil, nil
	}
	scope := g.scope(ctx)
	cat := g.cat()
	g.mu.Lock()
	var ended []liveGrant
	if st := g.scopes[scope]; g.scoped && st != nil && len(st.ended) > 0 {
		names := make([]string, 0, len(st.ended))
		for name := range st.ended {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if lg := st.ended[name]; cat != nil {
				if _, ok := cat.Lookup(lg.skill); ok {
					ended = append(ended, lg)
				}
			}
		}
	}
	g.mu.Unlock()
	if len(ended) == 0 {
		return nil, nil
	}
	args := info.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	subjects := []agentpolicy.Subject{{Args: args}}
	if m, ok := g.matchers[info.Call.Name]; ok && m.Subjects != nil {
		split, err := m.Subjects(args)
		if err != nil || len(split) == 0 {
			return nil, nil // the engine fails it closed
		}
		subjects = split
	}
	var skills []string
	for _, s := range subjects {
		tool := s.Tool
		if tool == "" {
			tool = info.Call.Name
		}
		covered := false
		for _, lg := range ended {
			for _, r := range lg.rules {
				if _, carve := r.CarveOut(); carve || !r.MatchesTool(tool) {
					continue
				}
				if m := g.matchers[tool].Match; r.Bare() || m != nil && m(r.Spec, s.Args) {
					covered = true
					if !slices.Contains(skills, lg.skill) {
						skills = append(skills, lg.skill)
					}
					break
				}
			}
			if covered {
				break
			}
		}
		if !covered {
			return nil, nil
		}
	}
	// The grants are revoked, so the engine decides as it would have
	// without them. The context says so to this hook, which the
	// evaluation folds in again.
	v, err := g.engine.Would(context.WithValue(agentpolicy.ContextWithGrantScope(ctx, scope), wouldKey{}, true), info)
	if err != nil {
		return nil, nil // the decision reports it
	}
	if v.Action != agentturn.Defer {
		return nil, nil
	}
	if v.Rule == nil && g.hooks {
		// No ask rule is behind it: the policy's default, or a hook the
		// product folded into the engine, which would defer the call
		// whatever a grant did. The verdict does not say which, so the
		// engine decides, and a hook's question is not answered with a
		// refusal naming a skill that would not have helped.
		return nil, nil
	}
	reason := "the tools skill " + skills[0] + " granted"
	if len(skills) > 1 {
		reason = "the tools skills " + strings.Join(skills, " and ") + " granted"
	}
	reason += " ended with the user's last message; read the skill again with the " + agentskill.ToolName + " tool, then make this call again"
	return &agentturn.ToolDecision{Action: agentturn.Block, By: agentpolicy.ByPolicy, Reason: reason}, nil
}

// wrap returns the catalogue's tool, or whatever stands in for it,
// granting on each read of a skill's own instructions.
func (g *skillGrants) wrap(t agenttool.Tool) agenttool.Tool {
	return agenttool.Wrap(t, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
		res, err := t.Execute(ctx, call)
		if err != nil {
			if errors.Is(err, agentskill.ErrSkillChanged) {
				g.changed(ctx, call, err)
			}
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
		// The grant belongs to the conversation that read the skill, the
		// grant scope of the call's context.
		ctx = g.scopedCtx(ctx)
		g.gmu.Lock()
		defer g.gmu.Unlock()
		sk, ok := g.cat().Lookup(read.Name)
		if !ok {
			return res, nil
		}
		// The skill is read again, so a refusal by its ended grant is
		// over, whatever the read grants: a skill whose allowed-tools
		// were emptied or broken since grants nothing, and must not go
		// on refusing the tool it no longer names.
		g.clearEnded(agentpolicy.GrantScopeFromContext(ctx), sk.ListedName())
		out := SkillGrant{Skill: sk.ListedName(), Location: sk.Location, FrontmatterChanged: read.FrontmatterChanged}
		rules, err := skillRules(sk)
		if err != nil {
			out.Err = fmt.Errorf("agentkit: skill %s: allowed-tools: %w", sk.ListedName(), err)
			g.tell(ctx, out)
			return res, nil
		}
		g.grant(ctx, sk, out, rules)
		return res, nil
	})
}

// changed reports a read the catalogue's tool refused because the skill
// file is gone, renamed or no longer parses since discovery,
// [agentskill.ErrSkillChanged]. The model is told to discover the skills
// again, which it cannot; the product can, through [Kit.ReloadSkills],
// so it hears the same. The grant of an earlier read stands, as
// ReloadSkills' doc says grants do. The name is the call's, parsed as
// the tool parses it, since no Read came back; a name the catalogue no
// longer lists is reported with no Location.
func (g *skillGrants) changed(ctx context.Context, call agenttool.Call, err error) {
	var args struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(call.Args, &args)
	out := SkillGrant{Skill: args.Name, Err: fmt.Errorf("agentkit: skill %s: read refused: %w", args.Name, err)}
	if sk, ok := g.cat().Lookup(args.Name); ok {
		out.Skill, out.Location = sk.ListedName(), sk.Location
		out.Err = fmt.Errorf("agentkit: skill %s: read refused: %w", sk.ListedName(), err)
	}
	g.tell(ctx, out)
}

// grant grants rules, the skill's or those of it a replay grants again,
// under the skill's source and reports out with what the engine did.
// Every read grants and reports: a repeated GrantSet under one source
// name replaces the set, so a read after a revoke puts the grant back
// and a read before one costs one engine call. A replay's GrantSet runs
// under a context the kit's observer passes over, since the verdicts it
// repeats are on the path already.
func (g *skillGrants) grant(ctx context.Context, sk *agentskill.Skill, out SkillGrant, rules []agentpolicy.Rule) {
	set := agentpolicy.RuleSet{Source: g.sourceOf(sk)}
	for _, r := range rules {
		set.Allow = append(set.Allow, agentpolicy.Rule{Tool: r.Tool, Spec: r.Spec, Source: set.Source})
	}
	if len(set.Allow) == 0 {
		return
	}
	ctx = g.scopedCtx(ctx)
	scope := agentpolicy.GrantScopeFromContext(ctx)
	if out.Replayed {
		ctx = context.WithValue(ctx, replayKey{}, true)
	}
	g.mu.Lock()
	st := g.state(scope)
	if st.sources == nil {
		st.sources = map[string]bool{}
	}
	st.sources[set.Source.Name] = true
	if parent, _ := ctx.Value(parentScopeKey{}).(string); parent != "" && parent != scope {
		g.link(parent, scope)
	}
	g.mu.Unlock()
	out.Granted, out.Refused = g.engine.GrantSet(ctx, set)
	var mark userMark
	bound := false
	if g.mark != nil {
		mark, bound = g.mark(ctx)
	}
	g.mu.Lock()
	st = g.state(scope)
	if st.live == nil {
		st.live = map[string]liveGrant{}
	}
	if len(out.Granted) > 0 {
		st.live[set.Source.Name] = liveGrant{skill: sk.ListedName(), source: set.Source.Name, rules: out.Granted, under: mark, bound: bound}
	} else {
		delete(st.live, set.Source.Name)
	}
	g.mu.Unlock()
	g.tell(ctx, out)
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
// another location is passed over. One whose digest is not what the
// catalogue's tool serves for the name now, or whose frontmatter digest
// is not the skill's, is not the skill the model read: it is left
// live, so a revocation after it still ends it, and what would have
// been granted at the end is instead reported with
// [ErrSkillGrantChanged] and recorded as a verdict in the session.
//
// A restart may narrow a grant and never widen it. When the kit built
// the engine, and so recorded its verdicts, what is granted again is
// the rules the path says the engine granted for the read, the
// "granted <rule> by <source>" verdicts written just before its record,
// that the catalogue still gives the skill: a skill whose allowed-tools
// were widened, or whose source turned trusted, since the read gets
// nothing it did not have. A read the session records no verdict for,
// granted or refused, was made by a kit whose observer wrote nowhere,
// and is reported with [ErrSkillGrantUnrecorded] and not granted. Under
// WithEngine the kit records nothing, and what is granted is the
// catalogue's rules as they stand now.
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
	// A replayed grant is bound to the user message the transcript an
	// agent is seeded with ends under, session.Transcript, the same items
	// Kit.AgentOptions gives agentturn.New, so the first turn after the
	// restart does not end it. Not the path's items: a fold's summary
	// stands in for the messages it folded there, a user-role message of
	// its own, and a restart after a fold past the last prompt would read
	// the prompt's digest as stale.
	if mark, ok := seededMark(sess); ok {
		ctx = context.WithValue(ctx, markKey{}, mark)
	}
	// The grants are the session's, made under the grant scope of its
	// conversation.
	ctx = g.scopedCtx(ctx)
	scope := agentpolicy.GrantScopeFromContext(ctx)
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
		// recorded is true when the read recorded the digest of the
		// frontmatter it was granted from and the catalogue's skill has
		// it still, so its allowed-tools are what the verdicts were about.
		recorded bool
		// seen is true when the path holds a verdict naming the source
		// since its last read record or revocation: the engine observes
		// one for every rule a GrantSet grants or refuses, so none means
		// the read's grant was recorded nowhere.
		seen bool
		// changed is set when the skill is not what the read recorded,
		// and frontmatter when it is the frontmatter's digest that moved.
		changed, frontmatter bool
	}
	var granted []live
	// pending is the rules each source was granted since its last read
	// record or revocation, which the next read record of it takes, and
	// seen the sources a verdict named since.
	pending := map[string]map[string]bool{}
	seen := map[string]bool{}
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
			sk, ok := g.cat().Lookup(read.Name)
			if !ok || sk.Location != read.Location {
				continue
			}
			source := g.sourceOf(sk).Name
			drop(source)
			l := live{name: read.Name, source: source, rules: pending[source], seen: seen[source], recorded: read.FrontmatterSHA256 != ""}
			delete(pending, source)
			delete(seen, source)
			// A read recorded before agentskill v0.0.10 has no frontmatter
			// digest, and the instructions' digest does not cover
			// allowed-tools.
			switch {
			case servedSHA(sk) != read.SHA256:
				l.changed = true
			case read.FrontmatterSHA256 != "" && read.FrontmatterSHA256 != sk.FrontmatterSHA256():
				l.changed, l.frontmatter = true, true
			}
			if g.observe != nil && l.rules == nil {
				l.rules = map[string]bool{}
			}
			granted = append(granted, l)
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
			if v.Source != "" {
				seen[v.Source] = true
			}
			switch {
			case v.Reason == revokedScopePrefix+scope:
				// Engine.RevokeScope ended every grant of the conversation.
				granted, pending, seen = nil, map[string]map[string]bool{}, map[string]bool{}
			case strings.HasPrefix(v.Reason, revokedPrefix):
				source := strings.TrimPrefix(v.Reason, revokedPrefix)
				drop(source)
				delete(pending, source)
				delete(seen, source)
			case v.Action == "allow" && v.Rule != "" && strings.HasPrefix(v.Reason, "granted "):
				if pending[v.Source] == nil {
					pending[v.Source] = map[string]bool{}
				}
				pending[v.Source][v.Rule] = true
			}
		}
	}
	for _, l := range granted {
		sk, ok := g.cat().Lookup(l.name)
		if !ok {
			continue
		}
		out := SkillGrant{Skill: sk.ListedName(), Location: sk.Location, Replayed: true}
		if l.changed {
			what := "its instructions or the files in its directory"
			if l.frontmatter {
				what = "its frontmatter"
			}
			out.FrontmatterChanged = l.frontmatter
			out.Err = fmt.Errorf("%w: skill %s: %s changed since the read; a read grants by the skill as it is now", ErrSkillGrantChanged, sk.ListedName(), what)
			if g.observe != nil {
				// On ctx as it is, not a replay's: the observer passes a
				// replay over, and this is not a verdict the path holds.
				g.observe(ctx, agentpolicy.Verdict{Tool: agentskill.ToolName, Action: agentturn.Block, Reason: "not granted again the tools of skill " + sk.ListedName() + ": the skill changed since it was read", By: agentpolicy.ByPolicy})
			}
			g.tell(ctx, out)
			continue
		}
		rules, err := skillRules(sk)
		if err != nil {
			continue
		}
		if g.observe != nil && !l.seen && len(rules) > 0 {
			out.Err = fmt.Errorf("%w: skill %s: the session records no verdict for the grant its read made, so nothing was granted again; was the run's recorder on its context, ContextWithRecorder?", ErrSkillGrantUnrecorded, sk.ListedName())
			g.tell(ctx, out)
			continue
		}
		if l.rules != nil {
			rules = replayRules(rules, l.rules, l.recorded)
			if len(rules) == 0 && len(l.rules) > 0 {
				out.Err = fmt.Errorf("agentkit: skill %s: none of the rules the session recorded as granted for its read is among the skill's allowed-tools now, so nothing was granted again", sk.ListedName())
				g.tell(ctx, out)
				continue
			}
		}
		g.grant(ctx, sk, out, rules)
		if l.recorded {
			g.narrow(ctx, sk, l.rules)
		}
	}
}

// skillRules is the skill's allowed-tools as agentpolicy rules with no
// source.
func skillRules(sk *agentskill.Skill) ([]agentpolicy.Rule, error) {
	rules, err := sk.Rules()
	if err != nil {
		return nil, err
	}
	out := make([]agentpolicy.Rule, len(rules))
	for i, r := range rules {
		out[i] = agentpolicy.Rule{Tool: r.Tool, Spec: r.Spec}
	}
	return out, nil
}

// replayRules is what a restart grants again of a skill whose allowed-
// tools are rules, given the rule texts the session's verdicts say the
// read was granted. The engine records a rule as it granted it, after
// [agentpolicy.WithAliases] expanded it: a skill's Bash(git:*) is
// recorded as bash(git:*) under an alias of Bash to bash. When the read
// recorded the digest of the frontmatter it was granted from, and the
// skill still has it, recorded is true: allowed-tools are as they were,
// so the recorded rules are the read's grant as the engine spelled
// them, and each is granted again whose specifier one of the skill's
// rules has. Otherwise the skill's rules are matched by their own
// spelling, which a restart under an alias does not find, and grants
// nothing: an allowed-tools the session cannot vouch for is not granted
// as recorded.
func replayRules(rules []agentpolicy.Rule, granted map[string]bool, recorded bool) []agentpolicy.Rule {
	var out []agentpolicy.Rule
	if !recorded {
		for _, r := range rules {
			if granted[agentpolicy.Rule{Tool: r.Tool, Spec: r.Spec}.String()] {
				out = append(out, r)
			}
		}
		return out
	}
	specs := map[string]bool{}
	for _, r := range rules {
		specs[r.Spec] = true
	}
	texts := make([]string, 0, len(granted))
	for text := range granted {
		texts = append(texts, text)
	}
	sort.Strings(texts)
	for _, text := range texts {
		parsed, err := agentpolicy.ParseRules(text)
		if err != nil || len(parsed) != 1 || !specs[parsed[0].Spec] {
			continue
		}
		out = append(out, parsed[0])
	}
	return out
}

// narrow revokes a replayed grant the engine took wider than the session
// recorded, which happens only when a recorded rule's tool is itself an
// alias the engine expands again. A restart never widens a grant, so
// the whole grant goes and the report says why.
func (g *skillGrants) narrow(ctx context.Context, sk *agentskill.Skill, recorded map[string]bool) {
	source := g.sourceOf(sk).Name
	scope := g.scope(ctx)
	g.mu.Lock()
	var lg liveGrant
	ok := false
	if st := g.scopes[scope]; st != nil {
		lg, ok = st.live[source]
	}
	g.mu.Unlock()
	if !ok {
		return
	}
	for _, r := range lg.rules {
		if !recorded[agentpolicy.Rule{Tool: r.Tool, Spec: r.Spec}.String()] {
			g.engine.Revoke(context.WithValue(ctx, replayKey{}, true), source)
			g.mu.Lock()
			if st := g.scopes[scope]; st != nil {
				delete(st.live, source)
			}
			g.mu.Unlock()
			g.tell(ctx, SkillGrant{Skill: sk.ListedName(), Location: sk.Location, Replayed: true, Err: fmt.Errorf("agentkit: skill %s: granting again what the session recorded granted %s, which it did not record, so the grant was revoked", sk.ListedName(), r)})
			return
		}
	}
}

// servedSHA is the digest the catalogue's tool serves for a read of the
// skill's instructions now, [agentskill.Read] SHA256, and "" when it
// serves none.
func servedSHA(sk *agentskill.Skill) string {
	text, err := sk.Instructions()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
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

// revokedScopePrefix opens the reason of the verdict agentpolicy's
// Engine.RevokeScope records, which names the grant scope after it.
const revokedScopePrefix = "revoked the rules granted under "

// childSep joins a grant scope to the scope of a child agent run under
// it, see [Kit.childContext]. It only makes a name that reads well: the
// children of a scope are the ones recorded, never the scopes whose names
// begin with it.
const childSep = "/"

// revoke revokes every source a read granted under in the grant scope of
// ctx, and the grants of the child agents run under it, forgets what the
// kit kept of the scope, and returns the number of rules the engine
// removed. It is for a conversation that is over, [Kit.RevokeSkillGrants].
func (g *skillGrants) revoke(ctx context.Context) int {
	n, _ := g.revokeLive(ctx, true)
	return n + g.endChildren(ctx)
}

// revokeLive revokes the sources a read granted under in the grant scope
// of ctx, forgetting the scope's state too when drop is set, and returns
// the number of rules the engine removed and the grants that were in
// force, by skill, in the order of their sources: those the engine still
// held, which is what ended, so a grant the product revoked from the
// engine itself, with Engine.Revoke or Engine.RevokeScope, is not
// reported ended twice.
func (g *skillGrants) revokeLive(ctx context.Context, drop bool) (int, []liveGrant) {
	ctx = g.scopedCtx(ctx)
	scope := agentpolicy.GrantScopeFromContext(ctx)
	g.gmu.Lock()
	defer g.gmu.Unlock()
	g.mu.Lock()
	var (
		names []string
		live  map[string]liveGrant
	)
	if st := g.scopes[scope]; st != nil {
		names = make([]string, 0, len(st.sources))
		for name := range st.sources {
			names = append(names, name)
		}
		live, st.live = st.live, nil
		if drop {
			delete(g.scopes, scope)
		}
	}
	g.mu.Unlock()
	if len(names) == 0 {
		return 0, nil
	}
	sort.Strings(names)
	held := map[string]bool{}
	for _, set := range g.engine.GrantsFor(ctx) {
		held[set.Source.Name] = true
	}
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
	var ended []liveGrant
	for _, name := range names {
		if lg, ok := live[name]; ok && held[name] {
			ended = append(ended, lg)
		}
	}
	return n, ended
}

// endChildren ends every grant of the child agents run under the grant
// scope of ctx, [Kit.childContext], and their own children's, and
// forgets their scopes: a child is one run of a conversation, nothing
// revokes its grants when it ends, and a scope no run reaches again
// would be a rule set the engine consults for every decision for as long
// as it lives. The scopes are those recorded as the conversation's
// children, and the sets under them are the kit's own, so a product's
// that it made under one are ended too. It takes gmu as a revocation
// does, so a read that is granting in a child's scope is not half undone.
// It returns the number of rules the engine removed.
func (g *skillGrants) endChildren(ctx context.Context) int {
	scope := g.scope(ctx)
	g.gmu.Lock()
	defer g.gmu.Unlock()
	g.mu.Lock()
	children := g.descendants(scope)
	for _, child := range children {
		delete(g.scopes, child)
		delete(g.children, child)
	}
	delete(g.children, scope)
	g.mu.Unlock()
	sort.Strings(children)
	n := 0
	for _, child := range children {
		n += g.engine.RevokeScope(agentpolicy.ContextWithGrantScope(ctx, child))
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
	// The hash is the frontmatter's, which the rules were parsed from,
	// so every verdict about the grant names what it was built from.
	return agentpolicy.Source{Name: "agentskill:" + sk.ListedName(), Path: sk.Location, Hash: sk.FrontmatterSHA256()}
}

// tell reports s, with the grant scope of ctx.
func (g *skillGrants) tell(ctx context.Context, s SkillGrant) {
	if g.report != nil {
		s.Scope = g.scope(ctx)
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
