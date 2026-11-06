package agentkit

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy/guard"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	childagent "github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/openresponses"
)

// ChildOption is a choice the kit makes for one child [WithChildAgent]
// offers. agentturn's own options for the child's tool go in through
// [ChildAgentOptions].
type ChildOption func(*childSettings)

type childSettings struct {
	opts  []childagent.Option
	parts []string
	share bool
}

// ChildAgentOptions passes agentturn/tools/agent's options to the
// child's tool, after the kit's own, so a childagent.WithObserver or
// childagent.WithRunContext given here still wins over the kit's.
func ChildAgentOptions(opts ...childagent.Option) ChildOption {
	return func(c *childSettings) { c.opts = append(c.opts, opts...) }
}

// ChildParts composes the child's instructions with [Kit.ChildConfig]:
// its own prompt as a part, then the kit's parts of the groups ids
// names, such as [PartMemory] or agentsmd.PartID, rendered again for
// each of the child's requests and recorded as parts. An id the kit does
// not order fails [New].
func ChildParts(ids ...string) ChildOption {
	return func(c *childSettings) { c.parts = append(c.parts, ids...) }
}

// ShareGrantScope runs the child under its conversation's grant scope,
// [Kit.GrantScope] of the parent's run, where a child otherwise has a
// scope of its own under it. The kit's other bindings stay: the
// recorder's ChildContext, the memory session and, under
// [WithSkillGrants], the parent's user-message mark. Passing a
// childagent.WithRunContext of the product's instead drops all of them.
//
// agentpolicy has no scope inheritance, so the scope is shared both
// ways. The child is decided by every set granted to the conversation, a
// person's "allow for this session" and a skill its parent read; and a
// skill the child reads under [WithSkillGrants] grants its tools to the
// conversation, the parent's calls too, bound to the parent's
// user-message mark and ended, under [WithSkillGrantScope], with the
// conversation's next message, as a read of the parent's is. A child that
// should see its parent's grants and keep its own reads to itself needs
// scope inheritance in agentpolicy, which there is not. Nothing is
// linked as the conversation's child, so [Kit.RevokeSkillGrants] ends
// what the child was granted as the conversation's own.
//
// A skill the child reads is recorded in the child's session, its read
// and its grant verdict, while the grant is the conversation's. A
// restart's regrant replays the conversation's own path, not its
// children's, so that grant is not restored: it fails closed, and the
// child, or the parent, reads the skill again to have it back.
func ShareGrantScope() ChildOption {
	return func(c *childSettings) { c.share = true }
}

// childComposition is one composition [Kit.ChildConfig] made: the
// child's own prompt part, the groups it takes, the parts of each
// config text it returned, and the parts its last request was sent,
// with what the layers left out of each. The kit's mu guards firsts,
// last and the omissions.
type childComposition struct {
	own  []Part
	want map[string]bool
	// pinned is a composition made during New, which is never evicted.
	pinned bool

	firsts      [][]Part
	firstOmits  [][]Omission
	last        []Part
	lastOmitted []Omission
}

// childCompositions bounds the runtime compositions the kit
// recognises, one per distinct prompt and set of parts [Kit.ChildConfig]
// was asked for after [New]. Those made during New, a [WithChildAgent]
// child's with [ChildParts] or a [WithDeferredTools] tool's, are pinned
// and not counted.
const childCompositions = 64

// touchChild marks the runtime composition under key as just asked for:
// last in recent, and past the bound the one asked for least recently
// is forgotten. The caller holds mu.
func (k *Kit) touchChild(key string) {
	if i := slices.Index(k.recent, key); i >= 0 {
		k.recent = slices.Delete(k.recent, i, i+1)
	}
	k.recent = append(k.recent, key)
	if len(k.recent) > childCompositions {
		delete(k.children, k.recent[0])
		k.recent = slices.Delete(k.recent, 0, 1)
	}
}

// ChildConfig returns cfg with its instructions composed from parts:
// cfg.Instructions as the child's own prompt, the part [PartProduct]
// from [SourceProduct], then the kit's parts of the groups ids names,
// in the kit's order, as the input guards left them. Each part keeps its
// source and its trust, so a child given the memory block records each
// entry as a part from agentmemory labelled as the entry is, untrusted
// unless the product said otherwise, and the child's path trust holds
// the label (agentsession RFC 0001, Instructions as parts: a writer that
// composes instructions from a layer of known trust records them as
// parts carrying it).
//
// The returned config's BeforeModelCall renders the selected parts
// again before each of the child's requests, as the kit's own requests
// are rendered: the memory block, a [WithPart] part and the MCP
// servers' instructions as they stand, the skill catalogue
// [Kit.ReloadSkills] last put in place, each guarded by the kit's
// input guards. A bounded group, the memory block and the servers'
// instructions, is sized against the child's instructions under
// [WithInstructionBudget], its prompt and the groups it takes, not the
// parent's, so a short child may be shown a block its parent's budget
// dropped. The child's own prompt passes the input guards too: in the
// config text, as [New] guards the parent's, with no observer and under
// the kit's own context, a refusal failing the call, and before each
// request with the groups. A memory render is recorded in the session the child's
// run writes, its own under [WithChildAgent]. It runs ahead of cfg's own
// BeforeModelCall, and only over instructions the composition made: a
// request whose instructions a per-call config or a hook rewrote is
// left as it is. [Kit.PartsFor] gives the parts of the returned text,
// which the recorder settles at the run's start, and of each request's
// render, so a recorder whose parts function is the kit's records the
// child's config entries as parts; under a recorder several kits share
// [PartsFrom] names this one. Concurrent runs of one composition share
// its last render, as the parent's runs share the kit's: a request from
// the other run's render is not recognised and is recorded with its
// instructions as one string.
//
// The kit recognises every composition made during [New], a
// [WithChildAgent] child's with [ChildParts] or one a
// [WithDeferredTools] function asked for, and of those made since, the
// 64 asked for most recently: past that, the one asked for least
// recently is forgotten, and a child still running on it is recorded
// with its instructions as one string. A product that composes per call
// keeps the prompt and ids the same across calls, which asks for one
// composition, not one each.
//
// ids are group IDs the kit orders: [PartProduct], [PartSkills],
// [PartMCP], [PartMemory], agentsmd.PartID and the [WithPart] ids. An
// id the kit does not order is refused, and so is [PartProduct] beside
// a prompt of the child's own, which would be two parts under one ID; a
// child with no prompt of its own may take the parent's. A group that
// is configured and empty adds nothing. [WithChildAgent] with [ChildParts]
// calls it for a [WithChildAgent] child; a product building its own
// child's tool calls it from [WithDeferredTools].
func (k *Kit) ChildConfig(cfg agentturn.Config, ids ...string) (agentturn.Config, error) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		switch {
		case !slices.Contains(k.order, id):
			return cfg, fmt.Errorf("agentkit: ChildConfig: %q is not a part the kit orders (%s)", id, strings.Join(k.order, ", "))
		case id == PartProduct && cfg.Instructions != "":
			return cfg, fmt.Errorf("agentkit: ChildConfig: %q beside the child's own prompt would put two parts under one ID", id)
		case want[id]:
			return cfg, fmt.Errorf("agentkit: ChildConfig: %q named twice", id)
		}
		want[id] = true
	}
	var own []Part
	if cfg.Instructions != "" {
		own = []Part{{ID: PartProduct, Text: cfg.Instructions, Source: SourceProduct}}
	}
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	key := cfg.Instructions + "\x00" + strings.Join(sorted, "\x00")

	// The child's prompt guarded as New guarded the parent's, with no
	// observer and outside the lock, since a guard may call a model; the
	// kit's parts are as the guards left them already.
	sentOwn := own
	if len(k.guards) > 0 && len(own) > 0 {
		var err error
		if sentOwn, err = guardParts(k.closeCtx, guard.Chain{Guards: k.guards}, own); err != nil {
			return cfg, fmt.Errorf("agentkit: ChildConfig: an input guard refused the child's prompt: %w", err)
		}
	}

	k.mu.Lock()
	first := append(slices.Clone(sentOwn), selectGroups(k.parts, want)...)
	omitted := k.childOmitted(want, k.partOmitted, k.mcpOmitted, k.memOmitted)
	c := k.children[key]
	if c == nil {
		if k.children == nil {
			k.children = map[string]*childComposition{}
		}
		c = &childComposition{own: own, want: want, pinned: !k.built}
		k.children[key] = c
	}
	if !c.pinned {
		k.touchChild(key)
	}
	if c.first(agentsession.JoinInstructions(first)) == nil {
		c.firsts = append(c.firsts, first)
		c.firstOmits = append(c.firstOmits, omitted)
		if len(c.firsts) > earlierConfigs {
			c.firsts, c.firstOmits = c.firsts[1:], c.firstOmits[1:]
		}
	}
	k.mu.Unlock()

	cfg.Instructions = agentsession.JoinInstructions(first)
	hook := k.childInstructions(c)
	if cfg.BeforeModelCall != nil {
		hook = agentturn.ChainBeforeModelCall(hook, cfg.BeforeModelCall)
	}
	cfg.BeforeModelCall = hook
	return cfg, nil
}

// childInstructions is the BeforeModelCall [Kit.ChildConfig] gives a
// child: the composition's groups rendered again over the kit's parts
// as they stand, guarded, after the child's own prompt.
func (k *Kit) childInstructions(c *childComposition) func(context.Context, *openresponses.Request) error {
	return func(ctx context.Context, req *openresponses.Request) error {
		k.mu.Lock()
		ours := c.knows(req.Instructions)
		base := k.base
		k.mu.Unlock()
		if !ours || k.rerender == nil {
			return nil
		}
		r, err := k.rerender(ctx, base, c.own, c.want)
		if err != nil {
			return err
		}
		parts := r.sent

		k.mu.Lock()
		c.last = parts
		c.lastOmitted = k.childOmitted(c.want, r.partOm, r.mcpOm, r.memOm)
		if r.memory {
			if id := agentturn.RunIDFromContext(ctx); id != "" {
				k.rendered.put(id, memoryRender{man: r.man, dropped: r.dropped})
			}
		}
		k.mu.Unlock()

		req.Instructions = agentsession.JoinInstructions(parts)
		if r.dropped {
			req.Tools = withoutMemoryWrites(req.Tools)
		}
		return nil
	}
}

// childOmitted is what the layers of the groups want left out: the
// skills' and AGENTS.md's, which change only on a reload, and the given
// renders' of the parts, the servers and the memory. The caller holds
// mu.
func (k *Kit) childOmitted(want map[string]bool, part, mcp, mem []Omission) []Omission {
	var out []Omission
	for _, list := range [][]Omission{k.omitted, part, mcp, mem} {
		for _, o := range list {
			if want[group(o.Part)] {
				out = append(out, o)
			}
		}
	}
	return out
}

// knows reports whether text is the composition's: a config text it
// returned, or its last request's. The caller holds the kit's mu.
func (c *childComposition) knows(text string) bool {
	return (c.last != nil && agentsession.JoinInstructions(c.last) == text) || c.first(text) != nil
}

// first returns the parts of a config text the composition returned,
// or nil. The caller holds the kit's mu.
func (c *childComposition) first(text string) []Part {
	for _, f := range c.firsts {
		if agentsession.JoinInstructions(f) == text {
			return f
		}
	}
	return nil
}

// childFor returns the parts and omissions of a composition whose
// last request or config text is text, or nil. The caller holds mu.
func (k *Kit) childFor(text string) ([]Part, []Omission) {
	for _, c := range k.children {
		if c.last != nil && agentsession.JoinInstructions(c.last) == text {
			return c.last, c.lastOmitted
		}
		for i, f := range c.firsts {
			if agentsession.JoinInstructions(f) == text {
				return f, c.firstOmits[i]
			}
		}
	}
	return nil, nil
}
