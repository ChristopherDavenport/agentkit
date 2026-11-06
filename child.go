package agentkit

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	childagent "github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/openresponses"
)

// ChildOption is a choice the kit makes for one child [WithChild]
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
// childagent.WithRunContext given here still wins, as it does through
// [WithChildAgent].
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
func ShareGrantScope() ChildOption {
	return func(c *childSettings) { c.share = true }
}

// WithChild is [WithChildAgent] with the kit's own choices for the child
// beside agentturn's: [ChildParts], [ShareGrantScope] and
// [ChildAgentOptions].
// WithChildAgent(cfg, opts...) is WithChild(cfg, ChildAgentOptions(opts...)),
// and everything WithChildAgent says holds here. The tool's source label
// is the same, "WithChildAgent #<n> <name>", so a [WithToolFilter] or a
// [WithToolWrap] reads one child as it reads the other.
func WithChild(cfg agentturn.Config, opts ...ChildOption) Option {
	var c childSettings
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	return func(s *settings) {
		s.deferTools = append(s.deferTools, deferred{
			kind: "WithChildAgent",
			name: cfg.Name,
			fn: func(k *Kit) ([]agenttool.Tool, error) {
				cfg := cfg
				if len(c.parts) > 0 {
					var err error
					if cfg, err = k.ChildConfig(cfg, c.parts...); err != nil {
						return nil, err
					}
				}
				// A fresh slice: appending to the caller's options would
				// grow their array and make two child agents share it.
				all := make([]childagent.Option, 0, len(c.opts)+2)
				all = append(all,
					childagent.WithObserver(k.observeChild),
					childagent.WithRunContext(k.childContext(c.share)))
				all = append(all, c.opts...)
				return []agenttool.Tool{childagent.New(cfg, all...)}, nil
			},
		})
	}
}

// childComposition is one composition [Kit.ChildConfig] made: the
// child's own prompt part, the groups it takes, the parts of each
// config text it returned, and the parts its last request was sent,
// with what the layers left out of each. The kit's mu guards firsts,
// last and the omissions.
type childComposition struct {
	own  []Part
	want map[string]bool

	firsts      [][]Part
	firstOmits  [][]Omission
	last        []Part
	lastOmitted []Omission
}

// childCompositions bounds the compositions the kit recognises: one per
// distinct prompt and set of parts [Kit.ChildConfig] was asked for.
const childCompositions = 64

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
// input guards. A memory render is recorded in the session the child's
// run writes, its own under [WithChildAgent]. It runs ahead of cfg's own
// BeforeModelCall, and only over instructions the composition made: a
// request whose instructions a per-call config or a hook rewrote is
// left as it is. [Kit.PartsFor] gives the parts of the returned text,
// which the recorder settles at the run's start, and of each request's
// render, so a recorder whose parts function is the kit's records the
// child's config entries as parts; under a recorder several kits share
// [PartsFrom] names this one.
//
// ids are group IDs the kit orders: [PartProduct], [PartSkills],
// [PartMCP], [PartMemory], agentsmd.PartID and the [WithPart] ids. An
// id the kit does not order is refused, and so is [PartProduct] beside
// a prompt of the child's own, which would be two parts under one ID; a
// child with no prompt of its own may take the parent's. A group that
// is configured and empty adds nothing. [WithChild] with [ChildParts]
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

	k.mu.Lock()
	first := append(slices.Clone(own), selectGroups(k.parts, want)...)
	omitted := k.childOmitted(want, k.partOmitted, k.mcpOmitted, k.memOmitted)
	c := k.children[key]
	if c == nil {
		if k.children == nil || len(k.children) >= childCompositions {
			k.children = map[string]*childComposition{}
		}
		c = &childComposition{own: own, want: want}
		k.children[key] = c
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
		r, err := k.rerender(ctx, base, c.want)
		if err != nil {
			return err
		}
		parts := append(slices.Clone(c.own), r.sent...)

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
