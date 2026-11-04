package agentkit_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// pngData is an inline image, which a sidecar session stores as a blob
// and names by a sidecar: reference.
const pngData = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="

// lookAt is a user message holding text and an inline image.
func lookAt(text string) *openresponses.Message {
	return openresponses.UserMessage(&openresponses.InputText{Text: text}, &openresponses.InputImage{ImageURL: pngData})
}

// imagesIn is the image URLs the items hold.
func imagesIn(items []openresponses.Item) []string {
	var urls []string
	for _, item := range items {
		m, ok := item.(*openresponses.Message)
		if !ok {
			continue
		}
		for _, p := range m.Content {
			if img, ok := p.(*openresponses.InputImage); ok {
				urls = append(urls, img.ImageURL)
			}
		}
	}
	return urls
}

// A sidecar session's items name their media by sidecar: references,
// which agentturn/session reads back as data URLs only through the
// recorder's read options. Resuming one seeds the agent with the
// images, and a head move reads the branch it moves to the same way.
func TestASidecarSessionResumesAndMovesWithItsMedia(t *testing.T) {
	store := agentsession.NewMemoryStore()
	first, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "m"),
		agentkit.WithSession(store, agentsession.Header{Media: agentsession.MediaSidecar}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	agent := agentturn.New(first.Config(), first.AgentOptions()...)
	unsubscribe := first.Attach(agent)
	for _, text := range []string{"one", "two"} {
		if _, err := agent.Prompt(t.Context(), lookAt(text)); err != nil {
			t.Fatal(err)
		}
	}
	unsubscribe()
	for _, e := range first.Session().Entries() {
		if line, _ := agentsession.MarshalEntry(e); strings.Contains(string(line), "data:image") {
			t.Fatalf("entry %s holds the image inline; the session is not sidecar", e.Base().ID)
		}
	}

	second, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "m"),
		agentkit.WithResumedSession(store, first.SessionID()),
	)
	if err != nil {
		t.Fatalf("resuming a sidecar session: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if got := imagesIn(second.Transcript()); len(got) != 2 || !strings.HasPrefix(got[0], "data:image/png;base64,") {
		t.Fatalf("the resumed transcript's images = %v, want both as data URLs", got)
	}

	resumed := agentturn.New(second.Config(), second.AgentOptions()...)
	t.Cleanup(second.Attach(resumed))
	ctl, err := second.Control(resumed)
	if err != nil {
		t.Fatal(err)
	}
	var prompt string
	for _, e := range second.Session().Entries() {
		if it, ok := e.(*agentsession.ItemEntry); ok {
			if m, ok := it.Item.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
				prompt = e.Base().ID
				break
			}
		}
	}
	if err := ctl.ContinueFrom(t.Context(), prompt); err != nil {
		t.Fatalf("moving the head in a sidecar session: %v", err)
	}
	if got := imagesIn(ctl.State().Transcript); len(got) != 1 || !strings.HasPrefix(got[0], "data:image/png;base64,") {
		t.Fatalf("the moved transcript's images = %v, want the first prompt's as a data URL", got)
	}
}

// A grant replayed from a sidecar session is bound to the last user
// message of the transcript the agent is seeded with, read with the
// session's blobs, as it is in an inline session, so a message that
// reaches another kit after the restart still ends it. Read without the
// blobs the transcript failed, the grant was bound to no message, and
// the message another kit received left it in force: triage issued the
// refund under message 1's grant.
func TestAReplayedGrantInASidecarSessionIsBoundToItsLastUserMessage(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "refunds", "issue_refund")
	sessions := agentsession.NewMemoryStore()
	refunds := 0
	refund := agenttool.New("issue_refund", "refund a charge",
		func(context.Context, struct {
			Customer string `json:"customer"`
		}) (string, error) {
			refunds++
			return "refunded", nil
		})
	kits := func(triageModel, billingModel agentturn.Model, sess agentkit.Option) (*agentkit.Kit, *agentkit.Kit) {
		triage, err := agentkit.New(t.Context(),
			agentkit.WithModel(triageModel, "m"),
			agentkit.WithSkills(skills),
			agentkit.WithTools(transferTool("transfer_to_billing"), refund),
			agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
				Read:    []string{agentskill.ToolName, "transfer_to_billing"},
				Execute: []string{"issue_refund"},
			}), nil),
			agentkit.WithSkillGrants(trustedSkills),
			agentkit.WithSkillGrantScope(),
			sess,
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = triage.Close() })
		billing, err := agentkit.New(t.Context(),
			agentkit.WithModel(billingModel, "m"),
			agentkit.WithTools(transferTool("transfer_to_triage")),
			agentkit.WithRecorder(triage.Recorder()),
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = billing.Close() })
		return triage, billing
	}

	// Message 1, with a screenshot: triage reads the skill and hands
	// off; billing answers.
	triage, billing := kits(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"refunds"}`),
		callTurn("transfer_to_billing", `{"reason":"double charge"}`),
	}}, &scriptModel{turns: []func(*openresponses.Emitter) error{
		textTurn("the second charge is the duplicate"),
	}}, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir(), Media: agentsession.MediaSidecar}))
	agent := agentturn.New(triage.Config())
	unsubscribe := triage.Attach(agent)
	if _, err := agent.Prompt(t.Context(), lookAt("I was double charged, see the statement; I want a refund.")); err != nil {
		t.Fatal(err)
	}
	if err := agent.SetConfig(billing.Config()); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Continue(t.Context()); err != nil {
		t.Fatal(err)
	}
	unsubscribe()

	// A restart, then message 2 reaches billing, which hands back.
	triage, billing = kits(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("issue_refund", `{"customer":"c-1"}`),
		textTurn("done"),
	}}, &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("transfer_to_triage", `{"reason":"refund it"}`),
	}}, agentkit.WithResumedSession(sessions, triage.SessionID()))
	if got := len(triage.Engine().Grants()); got != 1 {
		t.Fatalf("grants after the restart = %d, want the skill's", got)
	}
	agent = agentturn.New(billing.Config(), triage.AgentOptions()...)
	defer triage.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("Please just refund it.")); err != nil {
		t.Fatal(err)
	}
	if err := agent.SetConfig(triage.Config()); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Continue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(triage.Engine().Grants()); got != 0 || refunds != 0 {
		t.Fatalf("grants after message 2 = %d, refunds = %d; want the replayed grant ended by the message and the refund refused", got, refunds)
	}
}
