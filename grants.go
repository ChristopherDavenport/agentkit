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
	// Err is set when nothing was granted: because the skill's
	// allowed-tools would not parse; because the read was made in a
	// conversation the kit's grants do not belong to,
	// [ErrSkillGrantConversation]; because the catalogue's tool refused
	// the read, the skill file gone, renamed or unparseable since
	// discovery, [agentskill.ErrSkillChanged], a report a front reloads
	// on with [Kit.ReloadSkills]; or, with Replayed set, because a
	// restart would not grant a read again: the skill changed since the
	// read, [ErrSkillGrantChanged], the session records no verdict of
	// the read's grant, [ErrSkillGrantUnrecorded], or none of the rules
	// the session recorded for the read is among the skill's now. A
	// report with no Skill is the kit's grants ending because it decided
	// a call in another conversation; its Err wraps
	// ErrSkillGrantConversation and names both.
	Err error
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
// conversations gives each its own kit.
//
// A run's conversation is the session of the recorder on its context,
// [ContextWithRecorder]. Without one, it is the session
// session.ContextWithSessionID names on the context, as a front that
// records each conversation itself puts there, unless that is the kit's
// own or a child's of a run the kit served; and otherwise the kit's own
// session. Under [WithRecorder] the kit records into a recorder another
// owns, every session of which it takes for one conversation. Runs that
// name no session and that the kit records nowhere are one conversation
// to the kit, which cannot tell them apart.
var ErrSkillGrantConversation = errors.New("agentkit: this kit's skill grants belong to another conversation")

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
type skillGrants struct {
	// cat is the catalogue as it stands, which [Kit.ReloadSkills] may
	// replace.
	cat    func() *agentskill.Catalog
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
	// mark is the user message a grant made under the context is bound
	// to, Kit.markFor, and false for a grant bound to none.
	mark func(context.Context) (userMark, bool)
	// matchers are WithPolicy's, which is how blockEnded reads a call's
	// subjects and matches an ended rule's specifier against them.
	matchers map[string]agentpolicy.ToolMatcher
	// extraOpts is set when WithPolicy was given agentpolicy options of
	// the product's, which may change how the engine decides in ways
	// its exported state does not show, so blockEnded refuses nothing.
	extraOpts bool
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
	// conversation, and tripBy, the conversation of that call. A name
	// stays in sources once granted: revoking one the engine no longer
	// holds is a no-op. live is the grants in force, by source, which a
	// revocation clears, for the note the scope gives the model. ended is
	// the grants the scope's last revocations ended, by source, newest
	// kept, which guard refuses a call by until the skill is read again.
	mu       sync.Mutex
	sources  map[string]bool
	live     map[string]liveGrant
	ended    map[string]liveGrant
	owner    string
	ownerRec *session.Recorder
	bound    bool
	shared   bool
	tripBy   string
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

// stale reports whether a live grant was made under a user message
// earlier than the one a transcript with mark now is under.
func (g *skillGrants) stale(now userMark) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, lg := range g.live {
		if lg.bound && lg.under.stale(now) {
			return true
		}
	}
	return false
}

// keepEnded remembers the grants the scope ended, by the source each
// was made under, for blockEnded.
func (g *skillGrants) keepEnded(ended []liveGrant) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended == nil {
		g.ended = map[string]liveGrant{}
	}
	for _, lg := range ended {
		g.ended[lg.source] = lg
	}
}

// pruneEnded forgets the ended grants of skills the catalogue no longer
// lists, after Kit.ReloadSkills: a skill deleted or renamed can be read
// again under no name, so its ended grant would refuse its tool for the
// rest of the conversation.
func (g *skillGrants) pruneEnded() {
	cat := g.cat()
	g.mu.Lock()
	defer g.mu.Unlock()
	for source, lg := range g.ended {
		if _, ok := cat.Lookup(lg.skill); !ok {
			delete(g.ended, source)
		}
	}
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
// kit granting. [ErrSkillGrantConversation] says why. The scope's
// refusal of a call an ended grant allowed is not here but inside the
// engine, [blockEnded], so a batch is held as the engine holds it.
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
	g.shared, g.tripBy = true, conv
	rec, owner := g.ownerRec, g.owner
	g.mu.Unlock()
	// Written where the grants were made: the owner's run is not this
	// one, so the context carries the owner's recorder and nothing of the
	// run that tripped it. The cause goes first, so the revocations after
	// it are not read as a scope's or the product's.
	octx := ContextWithRecorder(context.WithoutCancel(context.Background()), rec)
	if g.observe != nil {
		g.observe(octx, agentpolicy.Verdict{Action: agentturn.Block, Reason: fmt.Sprintf("skill grants ended: the kit decided a call in session %q, and its skill grants belong to one conversation", conv), By: agentpolicy.ByPolicy})
	}
	g.revoke(octx)
	g.tell(SkillGrant{Err: fmt.Errorf("%w: the kit decided a call in session %q, so it revoked the grants of session %q and grants nothing after", ErrSkillGrantConversation, conv, owner)})
	return nil, nil
}

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
// or the call itself, is covered by a rule of an ended grant: the rule
// names the subject's tool and is bare or its specifier matches the
// subject under the tool's matcher. An ended grant of a skill the
// catalogue no longer lists covers nothing.
//
// The engine has no side-effect-free evaluation to ask whether it would
// allow, so the kit refuses only when the engine's exported state leaves
// it no way to: for every tool of the call's subjects, every rule naming
// that tool in [agentpolicy.Engine.Policy]'s lists and in every
// [agentpolicy.Engine.Grants] set is a bare ask, with no specifier and
// no carve-out; when no rule names the tool, the default does not
// allow; the call's tool does not say it runs confined,
// [agenttool.ConfinedBy], which lets a call past a bare ask; and
// WithPolicy was given no agentpolicy option of the product's, which
// may change how the engine reads any of that. Any specifier, any allow
// or deny naming a tool, any option: the call is left to the engine,
// which asks, allows or denies as it would have. The predicate collapses
// to an evaluation the engine exports once agentpolicy has one.
func (g *skillGrants) blockEnded(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	if info.Call == nil {
		return nil, nil
	}
	conv := g.conv(ctx)
	cat := g.cat()
	g.mu.Lock()
	owned := g.scoped && g.bound && !g.shared && g.owner == conv && len(g.ended) > 0
	names := make([]string, 0, len(g.ended))
	for name := range g.ended {
		names = append(names, name)
	}
	ended := make([]liveGrant, 0, len(names))
	sort.Strings(names)
	for _, name := range names {
		if lg := g.ended[name]; cat != nil {
			if _, ok := cat.Lookup(lg.skill); ok {
				ended = append(ended, lg)
			}
		}
	}
	g.mu.Unlock()
	if !owned || len(ended) == 0 {
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
	var skills, tools []string
	for _, s := range subjects {
		tool := s.Tool
		if tool == "" {
			tool = info.Call.Name
		}
		if !slices.Contains(tools, tool) {
			tools = append(tools, tool)
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
	if g.engineDecides(ctx, info, tools, args) {
		return nil, nil
	}
	reason := "the tools skill " + skills[0] + " granted"
	if len(skills) > 1 {
		reason = "the tools skills " + strings.Join(skills, " and ") + " granted"
	}
	reason += " ended with the user's last message; read the skill again with the " + agentskill.ToolName + " tool, then make this call again"
	return &agentturn.ToolDecision{Action: agentturn.Block, By: agentpolicy.ByPolicy, Reason: reason}, nil
}

// engineDecides reports whether the engine's exported state leaves it a
// way to decide a call over tools other than by a bare ask, in which
// case the kit leaves the call to it; see blockEnded.
func (g *skillGrants) engineDecides(ctx context.Context, info agentturn.ToolCallInfo, tools []string, args json.RawMessage) bool {
	if g.extraOpts {
		return true
	}
	if info.Tool != nil {
		if ok, _ := agenttool.ConfinedBy(ctx, info.Tool, args); ok {
			return true
		}
	}
	p := g.engine.Policy()
	sets := g.engine.Grants()
	defaultAllows := false
	if action, _ := p.Default.Action(); action == agentturn.Allow {
		defaultAllows = true
	}
	for _, tool := range tools {
		named := false
		decides := func(list []agentpolicy.Rule, ask bool) bool {
			for _, r := range list {
				if !r.MatchesTool(tool) {
					continue
				}
				named = true
				if !ask || !r.Bare() {
					return true
				}
			}
			return false
		}
		if decides(p.Allow, false) || decides(p.Deny, false) || decides(p.Ask, true) {
			return true
		}
		for _, set := range sets {
			if decides(set.Allow, false) || decides(set.Deny, false) || decides(set.Ask, true) {
				return true
			}
		}
		if !named && defaultAllows {
			return true
		}
	}
	return false
}

// wrap returns the catalogue's tool, or whatever stands in for it,
// granting on each read of a skill's own instructions.
func (g *skillGrants) wrap(t agenttool.Tool) agenttool.Tool {
	return agenttool.Wrap(t, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
		res, err := t.Execute(ctx, call)
		if err != nil {
			if errors.Is(err, agentskill.ErrSkillChanged) {
				g.changed(call, err)
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
		g.gmu.Lock()
		defer g.gmu.Unlock()
		if conv, ok := g.claim(ctx); !ok {
			g.refuse(ctx, read.Name, conv)
			return res, nil
		}
		sk, ok := g.cat().Lookup(read.Name)
		if !ok {
			return res, nil
		}
		out := SkillGrant{Skill: sk.ListedName(), Location: sk.Location, FrontmatterChanged: read.FrontmatterChanged}
		rules, err := skillRules(sk)
		if err != nil {
			out.Err = fmt.Errorf("agentkit: skill %s: allowed-tools: %w", sk.ListedName(), err)
			g.tell(out)
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
func (g *skillGrants) changed(call agenttool.Call, err error) {
	var args struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(call.Args, &args)
	out := SkillGrant{Skill: args.Name, Err: fmt.Errorf("agentkit: skill %s: read refused: %w", args.Name, err)}
	if sk, ok := g.cat().Lookup(args.Name); ok {
		out.Skill, out.Location = sk.ListedName(), sk.Location
		out.Err = fmt.Errorf("agentkit: skill %s: read refused: %w", sk.ListedName(), err)
	}
	g.tell(out)
}

// refuse reports, and records in the reading conversation's session, a
// read whose grant the kit will not make because its grants belong to
// another conversation. The model still gets the skill's text; its
// calls are decided by the policy alone.
func (g *skillGrants) refuse(ctx context.Context, name, conv string) {
	sk, ok := g.cat().Lookup(name)
	if !ok {
		return
	}
	g.mu.Lock()
	owner, tripBy := g.owner, g.tripBy
	g.mu.Unlock()
	err := fmt.Errorf("%w: skill %s was read in session %q, the grants belong to %q, and the kit serves more than one conversation", ErrSkillGrantConversation, sk.ListedName(), conv, owner)
	if conv == owner {
		err = fmt.Errorf("%w: skill %s was read in session %q, which the grants belonged to, but the kit has decided a call in session %q since, and grants nothing after it", ErrSkillGrantConversation, sk.ListedName(), conv, tripBy)
	}
	if g.observe != nil {
		g.observe(ctx, agentpolicy.Verdict{Tool: agentskill.ToolName, Action: agentturn.Block, Reason: "not granted the tools of skill " + sk.ListedName() + ": the kit serves more than one conversation and its skill grants belong to one", By: agentpolicy.ByPolicy})
	}
	g.tell(SkillGrant{Skill: sk.ListedName(), Location: sk.Location, Err: err})
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
	if out.Replayed {
		ctx = context.WithValue(ctx, replayKey{}, true)
	}
	g.mu.Lock()
	if g.sources == nil {
		g.sources = map[string]bool{}
	}
	g.sources[set.Source.Name] = true
	// The skill is read again, so a refusal by its ended grant is over.
	delete(g.ended, set.Source.Name)
	g.mu.Unlock()
	out.Granted, out.Refused = g.engine.GrantSet(ctx, set)
	var mark userMark
	bound := false
	if g.mark != nil {
		mark, bound = g.mark(ctx)
	}
	g.mu.Lock()
	if g.live == nil {
		g.live = map[string]liveGrant{}
	}
	if len(out.Granted) > 0 {
		g.live[set.Source.Name] = liveGrant{skill: sk.ListedName(), source: set.Source.Name, rules: out.Granted, under: mark, bound: bound}
	} else {
		delete(g.live, set.Source.Name)
	}
	g.mu.Unlock()
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
			g.tell(out)
			continue
		}
		rules, err := skillRules(sk)
		if err != nil {
			continue
		}
		if g.observe != nil && !l.seen && len(rules) > 0 {
			out.Err = fmt.Errorf("%w: skill %s: the session records no verdict for the grant its read made, so nothing was granted again; was the run's recorder on its context, ContextWithRecorder?", ErrSkillGrantUnrecorded, sk.ListedName())
			g.tell(out)
			continue
		}
		if l.rules != nil {
			rules = replayRules(rules, l.rules, l.recorded)
			if len(rules) == 0 && len(l.rules) > 0 {
				out.Err = fmt.Errorf("agentkit: skill %s: none of the rules the session recorded as granted for its read is among the skill's allowed-tools now, so nothing was granted again", sk.ListedName())
				g.tell(out)
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
	g.mu.Lock()
	lg, ok := g.live[source]
	g.mu.Unlock()
	if !ok {
		return
	}
	for _, r := range lg.rules {
		if !recorded[agentpolicy.Rule{Tool: r.Tool, Spec: r.Spec}.String()] {
			g.engine.Revoke(context.WithValue(ctx, replayKey{}, true), source)
			g.mu.Lock()
			delete(g.live, source)
			g.mu.Unlock()
			g.tell(SkillGrant{Skill: sk.ListedName(), Location: sk.Location, Replayed: true, Err: fmt.Errorf("agentkit: skill %s: granting again what the session recorded granted %s, which it did not record, so the grant was revoked", sk.ListedName(), r)})
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

// revoke revokes every source a read granted under and returns the
// number of rules the engine removed.
func (g *skillGrants) revoke(ctx context.Context) int {
	n, _ := g.revokeLive(ctx)
	return n
}

// revokeLive is revoke, and returns too the grants that were in force,
// by skill, in the order of their sources.
func (g *skillGrants) revokeLive(ctx context.Context) (int, []liveGrant) {
	g.mu.Lock()
	names := make([]string, 0, len(g.sources))
	for name := range g.sources {
		names = append(names, name)
	}
	live := g.live
	g.live = nil
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
	var ended []liveGrant
	for _, name := range names {
		if lg, ok := live[name]; ok {
			ended = append(ended, lg)
		}
	}
	return n, ended
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
