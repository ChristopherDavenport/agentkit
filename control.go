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
//     held beside another call's ask is released with the batch, and
//     tells the engine how the resumed run went
//     ([agentpolicy.Engine.Resumed]);
//   - Queue writes the queued entry before it returns
//     ([session.Recorder.Queue]), so a steer is on the record before a
//     quiet tool's next event would write it;
//   - under [WithQuestionEvents], a question asked while a call of one
//     of its runs goes, a tool's own, a nested call the policy asks
//     about and an MCP server's, is an [agentturn.Question] event to
//     its subscribers, answered with Reply: each run carries its
//     Control ([ContextWithControl]), and the kit's elicitor asks the
//     Control of the run that asks.
//
// It is the composition agentconsole's kitbackend and dax each wrote
// for themselves, written once, from exported calls alone; docs/manual.md
// lists them. Kit.Control builds it.
type Control struct {
	kit   *Kit
	agent *agentturn.Agent

	// seedRun is the run that left pending the calls an agent seeded
	// from the kit's session holds, from the session's run end: the
	// agent reports no run until its first, and the engine keeps what it
	// held under the run that deferred it.
	seedRun string

	mu     sync.Mutex
	models agentturn.ReasoningModels // of the branch the head was moved to
}

var _ agentturn.Control = (*Control)(nil)

// Control returns the agent a as the human plane drives it, a built
// over k: agentturn.New(k.Config(), k.AgentOptions()...) and attached
// with [Kit.Attach]. Call it before the agent's first run; it refuses
// an agent that is running. A kit with a session of its own
// ([WithSession], [WithResumedSession], [WithRecorder]) records every
// run of every Control in that session, since RunContext puts the
// kit's recorder on each, so it serves one conversation. A kit with
// none serves several, each an agent of its own driven through its own
// Control, each run under the [ContextWithRecorder] of its
// conversation's session, which RunContext leaves in place.
//
//   - Under [WithQuestionEvents] the questions a tool asks mid-call
//     are events: the kit's config carries an elicitor that asks the
//     Control of the run the question comes from (the one on its
//     context, [ControlFromContext]), through that agent's
//     [agentturn.Agent.QuestionElicitor] with the run's recorder's
//     [session.Recorder.Elicitor] around it, by
//     [agentsession.ByHuman] ([Control.Elicitor]). Each conversation's
//     questions reach its own subscribers and no one else's; a run
//     that carries no Control, one started on the agent directly,
//     answers its questions cancelled, by [agentsession.ByPolicy],
//     and asks nobody. Control gives a an elicitor when its config has
//     none. A subscriber that cannot answer a question replies
//     [agenttool.ActionCancel], since the call waits for the reply or
//     for its context to end, and names itself on the reply's context
//     with [agentturn.ContextWithAnswerer], a trigger of its own kind,
//     so the record's answer entry says who answered (its source); the
//     entry's by stays [agentsession.ByHuman], which the elicitor was
//     built with.
//   - Without it the questions go where the kit's config says: to the
//     product's elicitor under [WithToolElicitor], which answers in
//     process, or unasked, agentturn's default, so a host with nobody
//     to answer never waits on one. A host that wants them answered by
//     rule passes WithToolElicitor(agentsession.ByPolicy, fn) with an
//     fn that answers, [agenttool.ActionCancel] for instance.
//   - It hands a the inputs a resumed session owes
//     ([session.Recorder.Requeue]), which the next run takes.
//
// It returns an error when a is running.
func (k *Kit) Control(a *agentturn.Agent) (*Control, error) {
	if a == nil {
		return nil, errors.New("agentkit: control: no agent")
	}
	if a.State().Running {
		return nil, fmt.Errorf("agentkit: control: %w", agentturn.ErrRunning)
	}
	c := &Control{kit: k, agent: a}
	if st := a.State(); st.RunID == "" && len(st.Pending) > 0 && k.sess != nil {
		c.seedRun = runOfPending(k.sess, st.Pending)
	}
	if k.questionEvents {
		if cfg := a.Config(); cfg.ToolElicitor == nil {
			k.mu.Lock()
			cfg.ToolElicitor = k.cfg.ToolElicitor
			k.mu.Unlock()
			if err := a.SetConfig(cfg); err != nil {
				return nil, fmt.Errorf("agentkit: control: %w", err)
			}
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

type controlKey struct{}

// ContextWithControl returns ctx carrying c, the Control a run started
// under it belongs to. [Control.RunContext] puts it on every run, and
// the values of a run's context reach its tools, its hooks and the
// sub-agents its tools run, so a question asked anywhere in the run
// finds the conversation it belongs to.
func ContextWithControl(ctx context.Context, c *Control) context.Context {
	return context.WithValue(ctx, controlKey{}, c)
}

// ControlFromContext returns the Control [ContextWithControl] put on
// ctx.
func ControlFromContext(ctx context.Context) (*Control, bool) {
	c, ok := ctx.Value(controlKey{}).(*Control)
	return c, ok && c != nil
}

// questions is the ToolElicitor [WithQuestionEvents] puts on the kit's
// config: the question goes to the Control of the run that asks, as an
// event to its subscribers, and is answered cancelled, by the policy,
// for a run that carries none. It asks no other conversation.
func (k *Kit) questions(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
	if c, ok := ControlFromContext(ctx); ok {
		return c.Elicitor()(ctx, q)
	}
	cancel := func(context.Context, agenttool.Elicitation) (agenttool.Answer, error) {
		return agenttool.Answer{Action: agenttool.ActionCancel}, nil
	}
	if rec := k.recorderFor(ctx); rec != nil {
		return rec.Elicitor(agentsession.ByPolicy, cancel)(ctx, q)
	}
	return cancel(ctx, q)
}

// Agent is the agent Control drives, for what the contract does not
// carry: its configuration, and SetConfig between runs.
func (c *Control) Agent() *agentturn.Agent { return c.agent }

// Elicitor is the elicitor the kit's questions reach for a run of this
// Control under [WithQuestionEvents]: the agent's QuestionElicitor,
// recorded by the run's recorder, the one on the call's context or the
// kit's, as answered by a person.
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

// RunContext puts on ctx what the kit expects of a run it drives: the
// Control itself ([ContextWithControl]), so the run's questions reach
// this conversation's subscribers; the kit's recorder, the session's
// ID for the model calls and for memory a run writes; and the reasoning
// attribution of the branch the head was last moved to. Prompt, Resume,
// Queue and ContinueFrom use it; a front that starts a run of its own
// on the agent does too. With a recorder of the kit's own it replaces
// any recorder the caller put on ctx; without one it adds the Control
// and the attribution alone, and a recorder the caller put there
// ([ContextWithRecorder]) stays, which is how one kit with no session
// serves several conversations, each recorded in its own.
func (c *Control) RunContext(ctx context.Context) context.Context {
	ctx = ContextWithControl(ctx, c)
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

// Prompt implements [agentturn.Control]: the agent's Prompt under
// RunContext. A prompt that opens with the outputs of the calls a run
// left pending answers them without a Resume, so what the kit's engine
// held of that run is settled with the run the prompt started: the
// calls its end still lists, those of a run that failed before the
// outputs went in, wait under the new run ([agentpolicy.Engine.Resumed]
// with no answers), and the rest of the old run is forgotten
// ([agentpolicy.Engine.Forget]), since nothing will release them.
func (c *Control) Prompt(ctx context.Context, items ...openresponses.Item) (*agentturn.RunEnd, error) {
	before := c.agent.State()
	ctx = c.RunContext(ctx)
	end, err := c.agent.Prompt(ctx, items...)
	if eng := c.kit.Engine(); eng != nil && end != nil && len(before.Pending) > 0 {
		run := c.pendingRun(before)
		was := &agentturn.RunEnd{RunID: run, Reason: agentturn.ReasonInputRequired, Pending: before.Pending}
		eng.Resumed(ctx, was, nil, end)
		eng.Forget(run)
	}
	return end, err
}

// pendingRun is the run the calls pending in st wait under in the
// engine: the agent's last, or for an agent seeded with them that has
// not run yet, the run that deferred them.
func (c *Control) pendingRun(st agentturn.State) string {
	if st.RunID != "" {
		return st.RunID
	}
	return c.seedRun
}

// Resume implements [agentturn.Control]: the agent's Resume under
// RunContext, with the answers put through the kit's engine. The
// engine is given the calls the agent has pending, as an end of the
// agent's last run ([agentturn.State]), so a call it held is released
// from the decision that deferred it ([agentturn.PendingCall]'s
// Decision, Held set), which a session read back after a restart, or an
// end from a wire, carries as the run's own did. A run that ended
// waiting on held calls alone, which [Control.Permissions] lists
// nothing for, is released by Resume with no answers.
//
// The release is in two phases ([agentpolicy.Engine.Release], then
// [agentpolicy.Engine.Resumed] with the end the resumed run returned):
// a Resume that did not start, or stopped with calls still pending,
// leaves the engine holding what it held, and the next Resume, under
// the run ID the agent then reports, releases them again. Answers
// Resume would refuse, one for a call that is not pending and two for
// one call, are refused first, and a pending call nobody answers is
// [agentpolicy.ErrUnanswered]; neither changes anything.
func (c *Control) Resume(ctx context.Context, answers ...agentturn.Answer) (*agentturn.RunEnd, error) {
	ctx = c.RunContext(ctx)
	eng := c.kit.Engine()
	if eng == nil {
		return c.agent.Resume(ctx, answers...)
	}
	st := c.agent.State()
	if err := checkAnswers(st.Pending, answers); err != nil {
		return nil, err
	}
	end := &agentturn.RunEnd{
		RunID:   c.pendingRun(st),
		Reason:  agentturn.ReasonInputRequired,
		Pending: st.Pending,
	}
	answers, err := eng.Release(ctx, end, answers...)
	if err != nil {
		return nil, err
	}
	next, err := c.agent.Resume(ctx, answers...)
	eng.Resumed(ctx, end, answers, next)
	return next, err
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
// given with the answer reaches the model, as what the person said,
// after the outputs of the batch the question was asked under, and the
// record. Who answered is the answerer the replier put on ctx with
// [agentturn.ContextWithAnswerer], which the record writes beside the
// answer; the run's trigger, which a Question's own event context
// carries, does not name the answerer, so a reply that names nobody is
// unattributed.
func (c *Control) Reply(ctx context.Context, id string, answer agenttool.Answer) error {
	return c.agent.Reply(ctx, id, answer)
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
// answers. The question, the part it is about and the held mark are
// the decision's that deferred the call ([agentturn.PendingCall]'s
// Decision, which agentpolicy fills from its verdict and
// agentturn/session reads back from the record), so an end read back
// after a restart or received over a wire gives the same list as one
// the engine decided. A call with no decision, one a Resume approved
// whose batch then failed before the call ran, is read from the
// engine's verdict under the end's run ([agentpolicy.Engine.Deferred]),
// where [agentpolicy.Engine.Resumed] moved it; one the engine does not
// hold either, a call an abort cut off, is listed with no question. It
// is nil for an end that is not [agentturn.ReasonInputRequired], and
// empty for one that waits on held calls alone: Resume with no answers
// releases those.
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
		switch d := p.Decision; {
		case d != nil:
			if d.Held {
				continue
			}
			perm.Reason, perm.Subject = d.Reason, d.Subject
		case eng != nil:
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
// the agent is given that branch's transcript, pending calls, the
// notes it owes ([session.OwedNotes]) and reasoning attribution, and a
// leaf label is appended so the move is on the record, for a follower
// and for a session reopened later. The
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
	prevModels := c.models
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
		if rerr := c.agent.SetNotes(prev.Notes); rerr != nil {
			errs = append(errs, rerr)
		}
		if rerr := c.kit.RegrantSkills(ctx, s); rerr != nil {
			errs = append(errs, rerr)
		}
		c.mu.Lock()
		c.models = prevModels
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
	// The notes the branch owes after outputs already on it, which a
	// failed run there could not append; with calls pending, Pending
	// put them on the last call instead.
	owed, err := session.OwedNotes(s)
	if err != nil {
		return fail(err)
	}
	if err := c.agent.SetNotes(owed); err != nil {
		return fail(err)
	}
	if err := c.kit.RegrantSkills(ctx, s); err != nil {
		return fail(err)
	}
	c.mu.Lock()
	c.models = models
	c.mu.Unlock()
	mark, err := s.MarkLeaf()
	if err != nil {
		return fail(err)
	}
	if _, err := rec.Store().Append(ctx, rec.SessionID(), mark); err != nil {
		return fail(fmt.Errorf("mark the head: %w", err))
	}
	if eng := c.kit.Engine(); eng != nil && len(prev.Pending) > 0 {
		// The calls the head left behind will not be answered: what
		// the engine held of their run goes with them.
		eng.Forget(prev.RunID)
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
