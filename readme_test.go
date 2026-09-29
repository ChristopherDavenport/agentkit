package agentkit_test

import (
	"regexp"
	"testing"
)

// The README's table of sibling versions is what a product author pins
// the siblings from, so it must say what go.mod requires. This compares
// the two in both directions, as TestEveryFieldTheKitSetsIsDocumented
// compares docs/manual.md to the code: every row names a required
// module at its required version, and every sibling go.mod requires has
// a row.
func TestTheREADMEVersionTableIsGoMod(t *testing.T) {
	const prefix = "github.com/ChristopherDavenport/"

	required := map[string]string{}
	req := regexp.MustCompile(`(?m)^\s*(?:require\s+)?` + regexp.QuoteMeta(prefix) + `(\S+)\s+(v\S+)`)
	for _, m := range req.FindAllStringSubmatch(readDoc(t, "go.mod"), -1) {
		required[m[1]] = m[2]
	}
	if len(required) == 0 {
		t.Fatal("go.mod requires no sibling; the comparison would be vacuous")
	}

	tabled := map[string]string{}
	row := regexp.MustCompile("(?m)^\\| (`[^|]+`) \\| (v[^ |]+) \\|$")
	name := regexp.MustCompile("`([^`]+)`")
	for _, m := range row.FindAllStringSubmatch(readDoc(t, "README.md"), -1) {
		for _, n := range name.FindAllStringSubmatch(m[1], -1) {
			tabled[n[1]] = m[2]
		}
	}

	for mod, v := range tabled {
		switch got, ok := required[mod]; {
		case !ok:
			t.Errorf("the README lists %s at %s and go.mod does not require it", mod, v)
		case got != v:
			t.Errorf("the README lists %s at %s and go.mod requires %s", mod, v, got)
		}
	}
	for mod, v := range required {
		if _, ok := tabled[mod]; !ok {
			t.Errorf("go.mod requires %s%s %s and the README's table has no row for it", prefix, mod, v)
		}
	}
}
