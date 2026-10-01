// Package a2a shows how an agentkit-built agent talks to a peer and
// serves as one, neither of which the kit has an option for.
//
// It needs none. [agentkit.Kit.Config] returns a plain
// [agentturn.Config], and both a2a packages take one, so a peer is an
// ordinary tool and serving is an ordinary front. The one seam is
// recording a served conversation in a session of its own, which
// [RecordEach] does with [agentkit.ContextWithRecorder].
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
	"sync"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	fronta2a "github.com/ChristopherDavenport/agentturn/front/a2a"
	"github.com/ChristopherDavenport/agentturn/session"
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

// RecordEach records every conversation the executor serves as a
// session of its own in store, for [Serve]'s opts: the first message of
// an a2a context starts one, and each later message resumes it.
//
// Pointing the agent's ToolRecorder at the conversation's recorder, as
// fronta2a's RecorderFor doc shows, reaches the records a tool writes.
// The rest of what a kit records, the policy's and the guards'
// verdicts, the memory manifest, a fold, a question a tool asked, is
// written by hooks the kit bound at New, and those follow the recorder
// [agentkit.ContextWithRecorder] puts on the run's context. Without it
// they land in the kit's own session, or nowhere, and the
// conversation's session has the calls but not the rules that let them
// run.
//
// A store holds a session it opened, a lock on it and the whole session
// in memory, until it is released, so the session is released when the
// last task running in its conversation ends, and the next message
// resumes it. Without that a server holds every session it has served,
// and no other process can open one: not the agentsession CLI, a second
// replica, or an auditor.
//
// One kit serves every conversation here, and two things it holds are
// the kit's rather than a conversation's: a skill grant, which the kit
// revokes the first time it serves a second conversation
// ([agentkit.ErrSkillGrantConversation]), and an MCP server's
// connection, whose identity every conversation shares. A server that
// needs either per conversation or per user builds a kit for each.
func RecordEach(kit *agentkit.Kit, store agentsession.Store, cwd string) fronta2a.Option {
	type conversation struct {
		id    string // the session ID
		tasks int    // the tasks running in it
	}
	var (
		mu    sync.Mutex
		convs = map[string]*conversation{} // by a2a context ID
	)
	release := func(contextID string) {
		mu.Lock()
		defer mu.Unlock()
		c := convs[contextID]
		if c.tasks--; c.tasks > 0 {
			return
		}
		if r, ok := store.(interface{ Release(string) error }); ok {
			_ = r.Release(c.id)
		}
	}
	return fronta2a.WithRecorderFor(func(ctx context.Context, contextID string, a *agentturn.Agent) (context.Context, func(), error) {
		parts := session.WithInstructionsParts(kit.PartsFor)
		// Held across the open, so two first messages of one context do
		// not each start a session.
		mu.Lock()
		defer mu.Unlock()
		c, seen := convs[contextID]
		var (
			rec *session.Recorder
			err error
		)
		if seen {
			rec, _, err = session.Resume(ctx, store, c.id, parts)
		} else {
			rec, _, err = session.Start(ctx, store, agentsession.Header{CWD: cwd}, parts)
		}
		if err != nil {
			return nil, nil, err
		}
		if !seen {
			c = &conversation{id: rec.SessionID()}
			convs[contextID] = c
		}

		cfg := a.Config()
		cfg.ToolRecorder = rec.RecordFunc()
		if err := a.SetConfig(cfg); err != nil {
			return nil, nil, err
		}
		c.tasks++
		detach := rec.Attach(a)
		ctx = session.ContextWithSessionID(ctx, rec.SessionID())
		return agentkit.ContextWithRecorder(ctx, rec), func() {
			detach()
			release(contextID)
		}, nil
	})
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
