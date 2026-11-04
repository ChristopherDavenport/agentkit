package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/openresponses"
)

// Control.Compact asks the kit's compaction transform to fold on the
// next request, whatever its budget, with instructions for that fold
// alone: a product's /compact. The fold is reported and recorded as an
// automatic one is. Without WithCompaction there is no transform to
// ask. (agentturn RFC 0001 The request, step 2; #110)
func TestControlCompactFoldsTheNextRequest(t *testing.T) {
	const keep = "keep the failing test names"
	model := &scriptModel{}
	var folds []compact.Fold
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithCompaction(1_000_000, compact.WithKeepLast(1), compact.WithMinFold(0)),
		agentkit.WithFoldObserver(func(_ context.Context, f compact.Fold) { folds = append(folds, f) }),
		agentkit.WithSession(agentsession.NewMemoryStore(), agentsession.Header{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
	defer kit.Attach(agent)()
	ctl, err := kit.Control(agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two"} {
		if _, err := ctl.Prompt(t.Context(), openresponses.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	if len(folds) != 0 {
		t.Fatalf("folded %d times under the budget before Compact", len(folds))
	}
	if err := ctl.Compact(t.Context(), keep); err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.Prompt(t.Context(), openresponses.UserText("three")); err != nil {
		t.Fatal(err)
	}
	if len(folds) != 1 || !folds[0].Forced || folds[0].Instructions != keep || folds[0].Err != nil {
		t.Fatalf("folds = %+v, want one asked for with the instructions", folds)
	}
	asked := false
	for _, req := range model.requests() {
		if req.Instructions != "" {
			continue // a turn's, not the summary's
		}
		in, _ := json.Marshal(req.Input)
		asked = asked || strings.Contains(string(in), keep)
	}
	if !asked {
		t.Error("the summary request does not carry the instructions")
	}
	recorded := 0
	for _, e := range kit.Session().Entries() {
		if _, ok := e.(*agentsession.CompactionEntry); ok {
			recorded++
		}
	}
	if recorded != 1 {
		t.Errorf("compactions recorded = %d, want the fold", recorded)
	}

	plain, err := agentkit.New(t.Context(), agentkit.WithModel(&scriptModel{}, "m"))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	pctl, err := plain.Control(agentturn.New(plain.Config()))
	if err != nil {
		t.Fatal(err)
	}
	if err := pctl.Compact(t.Context(), ""); !errors.Is(err, agentkit.ErrNoCompaction) {
		t.Fatalf("Compact without WithCompaction = %v, want ErrNoCompaction", err)
	}
}
