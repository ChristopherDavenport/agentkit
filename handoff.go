package agentkit

import (
	"context"
	"slices"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

// HandoffSource is the source label [Kit.Offer] gives the tools a
// handoff team adds to an agent's offer, its transfers: the label
// [WithToolFilter], [WithToolWrap] and a [Conflict] see them under.
const HandoffSource = "handoff"

// TeamConfig is [Kit.Config] for an agent of an agentturn/handoff team
// whose Offer is [TeamOffer]: the same configuration with no
// ToolProvider, since the team's Offer is where the kit's tools are
// offered, beside the transfers, through the kit's filter, wrappers,
// conflict check and policy, once. An agent built from it outside such
// a team is offered no tool of the kit's.
func (k *Kit) TeamConfig() agentturn.Config {
	cfg := k.Config()
	cfg.ToolProvider = nil
	return cfg
}

// Offer is the kit's offer of tools over a handoff team's list: the
// union of the kit's own sources and tools, the transfers the team adds
// under [HandoffSource], last, through the same [WithToolFilter], the
// same [WithToolWrap] and the kit's own wrappers, the same duplicate
// check and the same policy filter, under [Kit.GrantScope], as
// [Kit.Config]'s ToolProvider applies to the kit's own tools; and
// remembered for [Kit.LookupToolFor], as the provider's are. It has the
// signature of handoff.Team.Offer, which applies it on every request to
// the agent's list, so a transfer the policy denies is not offered and a
// product's wrapper reaches it (agentturn RFC 0001, Composition: a host
// offers transfers as it offers the agent's other tools).
//
// The pipeline runs once, here: the agent's config is [Kit.TeamConfig],
// whose own provider is gone, so the list the team passes holds only
// its transfers, and a tool of the kit's is not filtered and wrapped
// twice. A config whose provider is still the kit's, [Kit.Config],
// passes its tools again in tools, and they are offered twice over,
// the second time as handoff tools. A transfer wins its name: a kit
// tool that claims it too, such as a remote server's tool that appears
// at runtime, is dropped from this agent's offer and reported through
// [WithToolConflict] as a [Conflict] keeping [HandoffSource], since the
// transfers are the team's structure and the team routes a call of that
// name to the receiver (agentturn fails the turn on such a clash). The
// transfers are still offered after the kit's tools. A wrapper must keep a
// transfer's name and result, or the call runs without handing off; one
// that refuses the call is the way to stop a transfer.
//
// agent is not read: one kit serves one agent. [TeamOffer] picks the
// kit for each agent of a team.
func (k *Kit) Offer(ctx context.Context, _ string, tools []agenttool.Tool) []agenttool.Tool {
	ts := k.offerTools
	if ts == nil {
		return tools
	}
	extra := slices.Clone(tools)
	union := func(ctx context.Context) []agenttool.Tool {
		// The transfers first, so one wins a name a kit tool also
		// claims, and offered last, after the kit's own.
		sources := append([]source{{
			name: HandoffSource,
			live: func(context.Context) []agenttool.Tool { return extra },
		}}, ts.current()...)
		out, origins, conflicts := ts.resolveSources(ctx, sources)
		if len(conflicts) > 0 && ts.onConflict != nil {
			ts.report(conflicts)
		}
		var transfers int
		for transfers < len(origins) && origins[transfers].Source == HandoffSource {
			transfers++
		}
		out = append(slices.Clone(out[transfers:]), out[:transfers]...)
		k.remember(ctx, out)
		return out
	}
	if k.engine == nil {
		return union(ctx)
	}
	return k.engine.ToolProvider(union)(agentpolicy.ContextWithGrantScope(ctx, k.GrantScope(ctx)))
}

// TeamOffer is a handoff.Team's Offer for a team whose agents are kits:
// each agent's list goes through [Kit.Offer] of the kit whose
// [Kit.Config] carries its name, [WithName], and an agent no kit names
// is offered its list as it is. Each kit's agent is built from
// [Kit.TeamConfig]:
//
//	team, err := handoff.New(
//		handoff.Agent{Config: triage.TeamConfig(), Handoffs: []handoff.To{{Agent: "Billing"}}},
//		handoff.Agent{Config: billing.TeamConfig()},
//	)
//	team.Offer = agentkit.TeamOffer(triage, billing)
func TeamOffer(kits ...*Kit) func(ctx context.Context, agent string, tools []agenttool.Tool) []agenttool.Tool {
	byName := make(map[string]*Kit, len(kits))
	for _, k := range kits {
		if k == nil {
			continue
		}
		if name := k.Config().Name; name != "" {
			byName[name] = k
		}
	}
	return func(ctx context.Context, agent string, tools []agenttool.Tool) []agenttool.Tool {
		if k, ok := byName[agent]; ok {
			return k.Offer(ctx, agent, tools)
		}
		return tools
	}
}
