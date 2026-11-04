package agentkit_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// verdictlessStore refuses every policy verdict the recorder appends.
type verdictlessStore struct {
	*agentsession.MemoryStore
}

var errNoVerdicts = errors.New("the store refuses verdicts")

func (s verdictlessStore) Append(ctx context.Context, id string, e agentsession.Entry) (string, error) {
	if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == agentpolicy.VerdictNS {
		return "", errNoVerdicts
	}
	return s.MemoryStore.Append(ctx, id, e)
}

// A verdict the kit cannot write to the run's session fails the run, as
// a write of the recorder's own does: the session would otherwise say
// nothing of a decision the run went on under, and a restart replays
// skill grants from those verdicts. It was dropped, and the run went on.
func TestAVerdictTheSessionRefusesFailsTheRun(t *testing.T) {
	var ranBash atomic.Int32
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("bash", `{}`), textTurn("done")}}, "m"),
		agentkit.WithTools(counting("bash", &ranBash)),
		agentkit.WithPolicy(agentpolicy.Policy{Deny: policyRules(t, "bash"), Default: agentpolicy.Allow()}, nil),
		agentkit.WithSession(verdictlessStore{agentsession.NewMemoryStore()}, agentsession.Header{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
	defer kit.Attach(agent)()
	end, err := agent.Prompt(t.Context(), openresponses.UserText("run it"))
	if err == nil && end != nil {
		err = end.Err
	}
	if !errors.Is(err, errNoVerdicts) || !strings.Contains(err.Error(), "verdict") {
		t.Fatalf("run ended %+v, err %v; want it failed on the verdict the store refused", end, err)
	}
	if ranBash.Load() != 0 {
		t.Fatal("the denied call ran")
	}
}
