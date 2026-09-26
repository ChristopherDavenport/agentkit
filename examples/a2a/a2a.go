// Package a2a shows how an agentkit-built agent talks to a peer and
// serves as one, neither of which the kit has an option for.
//
// It needs none. [agentkit.Kit.Config] returns a plain
// [agentturn.Config], and both a2a packages take one, so a peer is an
// ordinary tool and serving is an ordinary front. There is no seam
// here for the kit to own.
//
// This is a nested module for the reason the kit has no option:
// agentturn/tools/a2a and agentturn/front/a2a are nested modules on
// a2aproject/a2a-go, and importing either takes a build from 64
// packages to 201 — gRPC, protobuf, genproto, x/net, x/text. Keeping
// them here means the code below is compiled and vetted by CI, like
// any other example in the repository, while a consumer of agentkit
// that never speaks a2a does not carry a gRPC stack to get it.
package a2a

import (
	"context"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	fronta2a "github.com/ChristopherDavenport/agentturn/front/a2a"
	toola2a "github.com/ChristopherDavenport/agentturn/tools/a2a"
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
)

// Peer dials a remote agent and returns it as a tool, for
// [agentkit.WithTools]. It joins the kit's namespace like any other
// tool, so a name it shares with a built-in is the same Conflict at
// New, and a policy filters it the same way.
//
//	peer, err := a2a.Peer(ctx, card)
//	kit, err := agentkit.New(ctx,
//		agentkit.WithModel(model, "gpt-5"),
//		agentkit.WithTools(read, write, peer),
//	)
func Peer(ctx context.Context, card *a2a.AgentCard, opts ...toola2a.Option) (agenttool.Tool, error) {
	client, err := a2aclient.NewFromCard(ctx, card)
	if err != nil {
		return nil, err
	}
	return toola2a.New(client, card, opts...), nil
}

// Serve returns the agent card and the executor that make a kit's
// agent reachable as an a2a peer.
//
// Both take the config the kit assembled, with no argument the kit had
// to add: the card's name and description are Config.Name and
// Config.Description, which is what [agentkit.WithName] sets, and its
// skills are the tools the kit unioned. An agent assembled by the kit
// and one assembled by hand serve identically, which is the whole
// point of the kit's rule.
func Serve(ctx context.Context, kit *agentkit.Kit, url, version string, opts ...fronta2a.Option) (*a2a.AgentCard, *fronta2a.Executor) {
	cfg := kit.Config()
	return fronta2a.AgentCard(ctx, cfg, url, version), fronta2a.New(cfg, opts...)
}

// Both is the round trip: an agent that calls a peer and is one.
func Both(ctx context.Context, model agentturn.Model, peerCard *a2a.AgentCard) (*agentkit.Kit, *a2a.AgentCard, *fronta2a.Executor, error) {
	peer, err := Peer(ctx, peerCard)
	if err != nil {
		return nil, nil, nil, err
	}
	kit, err := agentkit.New(ctx,
		agentkit.WithName("dex", "A coding agent that can delegate to a researcher."),
		agentkit.WithModel(model, "gpt-5"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithTools(peer),
	)
	if err != nil {
		return nil, nil, nil, err
	}
	card, exec := Serve(ctx, kit, "https://me.example/a2a", "1.0")
	return kit, card, exec, nil
}
