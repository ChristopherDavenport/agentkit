package agentkit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// Control is an agent built over a kit, driven as agentturn's
// [agentturn.Control]: the human plane a front, or a controller that
// answers by rule, holds. It does around each run what a kit's agent
// needs and a bare *agentturn.Agent does not:
//
//   - every run (Prompt, Resume), every queued input and every head
//     move carries the kit's recorder ([ContextWithRecorder]) and its
//     session ID ([session.ContextWithSessionID],
//     [agentmemory.WithSession]), so the verdicts, a tool's question
//     and its answer, memory and skill grants are recorded in, and
//     decided for, the kit's session;
//   - Resume puts the answers through the kit's engine
//     ([agentpolicy.Engine.Release]) first, so a call the engine only
//     held beside another call's ask is released with the batch;
//   - Queue writes the queued entry before it returns
//     ([session.Recorder.Queue]), so a steer is on the record before a
//     quiet tool's next event would write it;
//   - a question asked while a call runs is an [agentturn.Question]
//     event, answered with Reply, when the kit has no elicitor of its
//     own; an MCP server's too under [WithQuestionEvents].
//
// It is the composition agentconsole's kitbackend and dax each wrote
// for themselves, written once, from exported calls alone; docs/manual.md
// lists them. Kit.Control builds it.
type Control struct {
	kit   *Kit
	agent *agentturn.Agent

	mu      sync.Mutex
	lastEnd *agentturn.RunEnd         // how the last run ended, for the release
	models  agentturn.ReasoningModels // of the branch the head was moved to
}

var _ agentturn.Control = (*Control)(nil)

// Control returns the agent a as the human plane drives it, a built
// over k: agentturn.New(k.Config(), k.AgentOptions()...) and attached
// with [Kit.Attach]. Call it before the agent's first run. It does two
// things at once:
//
//   - When k has no elicitor of its own ([WithToolElicitor]), the
//     questions a tool asks mid-call become events: it sets
//     [Control.Elicitor], the agent's
//     [agentturn.Agent.QuestionElicitor] with the run's recorder's
//     [session.Recorder.Elicitor] around it, by
//     [agentsession.ByHuman], as the ToolElicitor of a's config and of
//     every [Kit.Config] after, so a config the product applies again
//     with [agentturn.Agent.SetConfig] (after [Kit.ReloadSkills], say)
//     keeps it. A tool's question and a nested call the policy asks
//     about are then recorded under the call and delivered to the
//     subscribers as [agentturn.Question]; an MCP server's too under
//     [WithQuestionEvents], which offers the servers elicitation. A
//     subscriber that cannot answer one replies
//     [agenttool.ActionCancel], since the call waits for the reply or
//     for its context to end. The kit's questions go to one agent, so
//     Control refuses a second agent once it has installed the first's.
//     A kit with its own elicitor answers in process, and the configs
//     are left as they are.
//   - It hands a the inputs a resumed session owes
//     ([session.Recorder.Requeue]), which the next run takes.
//
// It returns an error when a is running.
func (k *Kit) Control(a *agentturn.Agent) (*Control, error) {
	if a == nil {
		return nil, errors.New("agentkit: control: no agent")
	}
	c := &Control{kit: k, agent: a}
	k.mu.Lock()
	install := k.asker == a || (k.asker == nil && k.cfg.ToolElicitor == nil)
	if k.asker != nil && k.asker != a {
		k.mu.Unlock()
		return nil, errors.New("agentkit: control: the kit's questions already go to another agent's Control")
	}
	var prev agenttool.Elicitor
	if install {
		prev = k.cfg.ToolElicitor
		if k.asker == nil {
			k.cfg.ToolElicitor, k.asker = c.Elicitor(), a
		}
	}
	elicitor := k.cfg.ToolElicitor
	k.mu.Unlock()
	if install {
		cfg := a.Config()
		cfg.ToolElicitor = elicitor
		if err := a.SetConfig(cfg); err != nil {
			k.mu.Lock()
			if prev == nil {
				k.cfg.ToolElicitor, k.asker = nil, nil
			}
			k.mu.Unlock()
			return nil, fmt.Errorf("agentkit: control: %w", err)
		}
	}
	if rec := k.Recorder(); rec != nil {
		// Each owed input is on the path already, so its queued event
		// writes nothing; one already handed over is not handed over
		// again.
		rec.Requeue(context.Background(), a)
	}
	return c, nil
}

// Agent is the agent Control drives, for what the contract does not
// carry: its configuration, and SetConfig between runs.
func (c *Control) Agent() *agentturn.Agent { return c.agent }

// Elicitor is the elicitor Kit.Control installs: the agent's
// QuestionElicitor, recorded by the run's recorder, the one on the
// call's context or the kit's, as answered by a person.
func (c *Control) Elicitor() agenttool.Elicitor {
	ask := c.agent.QuestionElicitor()
	return func(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
		rec := RecorderFromContext(ctx)
		if rec == nil {
			rec = c.kit.Recorder()
		}
		if rec == nil {
			return ask(ctx, q)
		}
		return rec.Elicitor(agentsession.ByHuman, ask)(ctx, q)
	}
}

// RunContext puts on ctx what the kit expects of a run it records: its
// recorder, the session's ID for the model calls and for memory a run
// writes, and the reasoning attribution of the branch the head was last
// moved to. Prompt, Resume, Queue and ContinueFrom use it; a front that
// starts a run of its own on the agent does too. Without a recorder it
// adds only the attribution.
func (c *Control) RunContext(ctx context.Context) context.Context {
	c.mu.Lock()
	models := c.models
	c.mu.Unlock()
	if models != nil {
		// Agent.SetTranscript cannot take it: the loop leaves another
		// model's reasoning out of a request by this one.
		ctx = agentturn.ContextWithReasoningModels(ctx, models)
	}
	rec := c.kit.Recorder()
	if rec == nil {
		return ctx
	}
	ctx = ContextWithRecorder(ctx, rec)
	ctx = session.ContextWithSessionID(ctx, rec.SessionID())
	// Memory a run writes names the session; the kit cannot put the ID
	// on a context that is the host's.
	return agentmemory.WithSession(ctx, rec.SessionID())
}

// ended keeps how a run ended. A run that returned no end (it failed
// before it began) leaves the one before it, whose calls still wait.
func (c *Control) ended(end *agentturn.RunEnd) {
	if end == nil {
		return
	}
	c.mu.Lock()
	c.lastEnd = end
	c.mu.Unlock()
}

// Prompt implements [agentturn.Control]: the agent's Prompt under
// RunContext.
func (c *Control) Prompt(ctx context.Context, items ...openresponses.Item) (*agentturn.RunEnd, error) {
	end, err := c.agent.Prompt(c.RunContext(ctx), items...)
	c.ended(end)
	return end, err
}

// Resume implements [agentturn.Control]: the agent's Resume under
// RunContext, with the answers put through the kit's engine first. The
// engine is given the end the last run left its calls pending with,
// unless none of the answers is about it (a restart restored the
// calls, or another run ended since): it answers the calls it only
// held beside the ones asked about, and fails, changing nothing, when
// one is left unanswered.
//
// Answers Resume would refuse are refused before the release, which
// forgets the calls it answers and records them released: one for a
// call that is not pending, and two for one call. When the release
// went through and the run then did not start, the end is dropped and
// the error says to answer every pending call again, since the engine
// holds none of them now.
func (c *Control) Resume(ctx context.Context, answers ...agentturn.Answer) (*agentturn.RunEnd, error) {
	ctx = c.RunContext(ctx)
	eng := c.kit.Engine()
	if eng == nil {
		end, err := c.agent.Resume(ctx, answers...)
		c.ended(end)
		return end, err
	}
	if err := checkAnswers(c.agent.State().Pending, answers); err != nil {
		return nil, err
	}
	c.mu.Lock()
	end := c.lastEnd
	c.mu.Unlock()
	if end != nil && !slices.ContainsFunc(answers, func(a agentturn.Answer) bool { return pendingIn(end, a.CallID) }) {
		end = nil
	}
	answers, err := eng.Release(ctx, end, answers...)
	if err != nil {
		return nil, err
	}
	ran, err := c.agent.Resume(ctx, answers...)
	if ran == nil && err != nil && end != nil {
		c.mu.Lock()
		c.lastEnd = nil
		c.mu.Unlock()
		return nil, fmt.Errorf("agentkit: the run did not start after the calls were released (answer every pending call again): %w", err)
	}
	c.ended(ran)
	return ran, err
}

// checkAnswers refuses answers Resume would refuse, before anything is
// done on their account.
func checkAnswers(pending []agentturn.PendingCall, answers []agentturn.Answer) error {
	seen := map[string]bool{}
	for _, a := range answers {
		if seen[a.CallID] {
			return fmt.Errorf("agentkit: two answers for call %s", a.CallID)
		}
		seen[a.CallID] = true
		if !slices.ContainsFunc(pending, func(p agentturn.PendingCall) bool { return p.Call != nil && p.Call.CallID == a.CallID }) {
			return fmt.Errorf("agentkit: answer for call %s, which is not pending", a.CallID)
		}
	}
	return nil
}

func pendingIn(end *agentturn.RunEnd, callID string) bool {
	return slices.ContainsFunc(end.Pending, func(p agentturn.PendingCall) bool { return p.Call != nil && p.Call.CallID == callID })
}

// Queue implements [agentturn.Control]: with a recorder, each item is
// written as a queued entry before the agent takes it
// ([session.Recorder.Queue]) rather than at the agent's queued event,
// which the run's next event delivers and a quiet tool can hold off.
func (c *Control) Queue(ctx context.Context, mode agentturn.QueueMode, items ...openresponses.Item) error {
	ctx = c.RunContext(ctx)
	if rec := c.kit.Recorder(); rec != nil {
		return rec.Queue(ctx, c.agent, mode, items...)
	}
	return c.agent.Queue(ctx, mode, items...)
}

// Abort implements [agentturn.Control].
func (c *Control) Abort() { c.agent.Abort() }

// State implements [agentturn.Control].
func (c *Control) State() agentturn.State { return c.agent.State() }

// Subscribe implements [agentturn.Control]. A subscriber registered
// after [Kit.Attach] sees an event after the recorder wrote its entry.
func (c *Control) Subscribe(fn func(context.Context, agentturn.Event) error) func() {
	return c.agent.Subscribe(fn)
}

// Reply implements [agentturn.Control]: the agent's Reply. A note
// given with the answer reaches the model when the question was about
// a call (agentturn.Ask), and the record.
func (c *Control) Reply(id string, answer agenttool.Answer) error {
	return c.agent.Reply(id, answer)
}

// Permission is a call a run left for someone to answer, with the
// policy's question.
type Permission struct {
	Call *openresponses.FunctionCall
	// Reason is the policy's question: the rule that asked, and why.
	Reason string
	// Subject is the part of a compound call it asks about, as the
	// tool's splitter wrote it; empty for a call nothing split.
	Subject string
}

// Permissions are the calls a run that ended for input left for
// someone, each with the policy's question, and not the calls the
// kit's engine only holds beside them, which Resume releases with the
// answers. The question is the engine's verdict when it remembers the
// call, and the decision that deferred it ([agentturn.PendingCall]'s
// Decision) otherwise. It is nil for an end that is not
// [agentturn.ReasonInputRequired].
func (c *Control) Permissions(end *agentturn.RunEnd) []Permission {
	if end == nil || end.Reason != agentturn.ReasonInputRequired {
		return nil
	}
	eng := c.kit.Engine()
	var out []Permission
	for _, p := range end.Pending {
		if p.Call == nil {
			continue
		}
		perm := Permission{Call: p.Call}
		if p.Decision != nil {
			perm.Reason = p.Decision.Reason
		}
		if eng != nil {
			if v, ok := eng.Deferred(end.RunID, p.Call.CallID); ok {
				if v.Held {
					continue
				}
				perm.Reason, perm.Subject = v.Reason, v.Subject
			}
		}
		out = append(out, perm)
	}
	return out
}

// ContinueFrom moves the head to the entry entryID of the kit's
// session: the recorder is rebased there ([session.Recorder.Rebase]),
// the agent is given that branch's transcript, pending calls and
// reasoning attribution, and a leaf label is appended so the move is on
// the record, for a follower and for a session reopened later. The
// conversation's skill grants are ended before the head leaves
// ([Kit.RevokeSkillGrants]) and granted again from the new branch's
// reads after ([Kit.RegrantSkills]), so a tool a skill on the old
// branch allowed is not allowed on the new one.
//
// The target is checked before anything changes: a leaf label, an
// entry on a fork's prefix above its base, and an entry that leaves
// function calls without an output are refused. A failure after
// anything moved puts back the leaf, the transcript, the pending calls
// and the grants it had, so the session stays usable. The run check
// and the move are not atomic (agentturn#220): a run that starts in
// between fails the move, and the undo puts it back.
//
// It needs a recorder that writes a store it can open the session
// from, and returns [agentturn.ErrRunning] while a run goes.
func (c *Control) ContinueFrom(ctx context.Context, entryID string) (err error) {
	rec := c.kit.Recorder()
	if rec == nil {
		return errors.New("agentkit: continue from: the kit records no session")
	}
	if entryID == "" {
		return errors.New("agentkit: continue from: no entry")
	}
	fail := func(err error) error { return fmt.Errorf("agentkit: continue from %s: %w", entryID, err) }
	if c.agent.State().Running {
		return agentturn.ErrRunning
	}
	s, err := rec.Store().Open(ctx, rec.SessionID())
	if err != nil {
		return fail(err)
	}
	if err := mayRestOn(s, entryID); err != nil {
		return fail(err)
	}
	if calls, err := s.PendingCalls(entryID); err != nil {
		return fail(err)
	} else if len(calls) > 0 {
		return fail(fmt.Errorf("it leaves %d tool call(s) without an output (%s); continue from the answer after it", len(calls), calls[0].Call.Name))
	}

	prevLeaf, prev := s.Leaf(), c.agent.State()
	c.mu.Lock()
	prevModels, prevEnd := c.models, c.lastEnd
	c.mu.Unlock()
	ctx = c.RunContext(ctx)
	// The revocation writes its verdicts to the branch being left, which
	// moves the leaf, so a failure from here on puts the head back.
	c.kit.RevokeSkillGrants(ctx)
	moved := false
	defer func() {
		if err == nil {
			return
		}
		var errs []error
		if moved {
			// The grants are the new branch's now: end them while the
			// recorder still writes there, off the path the head goes
			// back to.
			c.kit.RevokeSkillGrants(ctx)
		}
		if rerr := rec.Rebase(s, prevLeaf); rerr != nil {
			errs = append(errs, rerr)
		} else if mark, rerr := s.MarkLeaf(); rerr != nil {
			errs = append(errs, rerr)
		} else if _, rerr := rec.Store().Append(ctx, rec.SessionID(), mark); rerr != nil {
			errs = append(errs, rerr)
		}
		if rerr := c.agent.SetTranscript(prev.Transcript); rerr != nil {
			errs = append(errs, rerr)
		}
		if rerr := c.agent.SetPending(prev.Pending); rerr != nil {
			errs = append(errs, rerr)
		}
		if rerr := c.kit.RegrantSkills(ctx, s); rerr != nil {
			errs = append(errs, rerr)
		}
		c.mu.Lock()
		c.models, c.lastEnd = prevModels, prevEnd
		c.mu.Unlock()
		if len(errs) > 0 {
			err = fmt.Errorf("%w (and putting the head back failed: %w)", err, errors.Join(errs...))
		}
	}()

	if err := rec.Rebase(s, entryID); err != nil {
		return fail(err)
	}
	moved = true
	items, models, err := session.TranscriptModels(s)
	if err != nil {
		return fail(err)
	}
	pending, err := session.Pending(s, append(rec.ReadOptions(), session.WithContext(ctx))...)
	if err != nil {
		return fail(err)
	}
	if err := c.agent.SetTranscript(items); err != nil {
		return fail(err)
	}
	if err := c.agent.SetPending(pending); err != nil {
		return fail(err)
	}
	if err := c.kit.RegrantSkills(ctx, s); err != nil {
		return fail(err)
	}
	c.mu.Lock()
	c.models, c.lastEnd = models, nil
	c.mu.Unlock()
	mark, err := s.MarkLeaf()
	if err != nil {
		return fail(err)
	}
	if _, err := rec.Store().Append(ctx, rec.SessionID(), mark); err != nil {
		return fail(fmt.Errorf("mark the head: %w", err))
	}
	return nil
}

// mayRestOn repeats agentsession's rule for where the leaf may rest,
// which it does not export: an entry the session holds that is not a
// leaf label, and, in a fork, not an entry of the prefix above the
// base.
func mayRestOn(s *agentsession.Session, id string) error {
	e, ok := s.Entry(id)
	if !ok {
		return fmt.Errorf("%w: %s", agentsession.ErrNoEntry, id)
	}
	if l, ok := e.(*agentsession.LabelEntry); ok && l.Label != nil && *l.Label == agentsession.LeafLabel {
		return errors.New("a leaf label is a bookmark, not a place to continue from")
	}
	if base := s.Header().Base; base != "" && id != base && s.Prefix(id) {
		return errors.New("it is on the fork's prefix above the base; the fork can only continue from its base or its own entries")
	}
	return nil
}
