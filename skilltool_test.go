package agentkit_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
)

// The skill tool's facts claim follows the catalogue the tool serves:
// after ReloadSkills, a skill the reload added is claimed by the
// product's WithFileClaim, and a skill another source now holds is
// claimed at the location the call reads. The tool New offered claimed
// through the catalogue New discovered, so the new skill's file read
// went unclaimed, past a policy that holds skill reads to read's rules,
// and the moved one was claimed at the file it no longer serves.
// (agenttool RFC 0001 Binding and forwarding, #96)
func TestTheSkillToolsClaimFollowsReloadSkills(t *testing.T) {
	root := t.TempDir()
	first, second := filepath.Join(root, "a"), filepath.Join(root, "b")
	skillWithTools(t, first, "existing", "Read")
	skillWithTools(t, second, "deploy", "Read")
	type claim struct{ skill, location, file string }
	var claims []claim
	fn := func(_ context.Context, s *agentskill.Skill, file string) ([]agenttool.FactCall, error) {
		claims = append(claims, claim{s.Name, s.Location, file})
		args, _ := json.Marshal(map[string]string{"path": filepath.Join(filepath.Dir(s.Location), file)})
		return []agenttool.FactCall{{Tool: "read", Args: args}}, nil
	}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(first, second),
		agentkit.WithSkillTool(agentskill.WithFileClaim(fn)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	old, _ := kit.Catalog().Lookup("deploy")

	skillWithTools(t, first, "late", "Read")
	skillWithTools(t, first, "deploy", "Read")
	if err := kit.ReloadSkills(t.Context()); err != nil {
		t.Fatal(err)
	}
	moved, _ := kit.Catalog().Lookup("deploy")
	if moved.Location == old.Location {
		t.Fatalf("deploy did not move: %s", moved.Location)
	}
	tool, ok := kit.LookupTool(agentskill.ToolName)
	if !ok {
		t.Fatal("no skill tool")
	}
	for _, tc := range []struct{ name, location string }{
		{"late", ""},
		{"deploy", moved.Location},
	} {
		claims = nil
		args, _ := json.Marshal(map[string]string{"name": tc.name, "path": ".env"})
		if _, _, err := agenttool.FactsOf(t.Context(), tool, args); err != nil {
			t.Fatal(err)
		}
		if len(claims) != 1 || claims[0].skill != tc.name || (tc.location != "" && claims[0].location != tc.location) {
			t.Errorf("claims for %s's .env = %+v, want one through the reloaded catalogue (at %s)", tc.name, claims, tc.location)
		}
	}
}
