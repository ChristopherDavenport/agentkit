package agentkit

import (
	"context"
	"fmt"
	"sync"

	"github.com/ChristopherDavenport/agenttool"
)

// Conflict is two tools claiming one name. The kit unions four tool
// sets into one namespace and nothing below it detects a collision: two
// tools with one name reach the model as two entries and the loop
// dispatches whichever the set returns first.
type Conflict struct {
	// Name is the name both tools claim.
	Name string
	// Kept is where the tool the model is offered came from.
	Kept string
	// Dropped is where the tool that is not offered came from.
	Dropped string
}

func (c Conflict) Error() string {
	return fmt.Sprintf("agentkit: two tools named %q: one from %s and one from %s", c.Name, c.Kept, c.Dropped)
}

// source is one contributor to the tool set, in the order the kit
// unions them: the product's own tools, the skill catalogue's tool, the
// memory tools, each MCP server's, and last a provider the product
// gave. The order is stable so a listing is reproducible.
type source struct {
	// name is what a conflict blames, such as "WithTools" or
	// "mcp:some-server".
	name string
	// tools is the fixed set, for a source that has one.
	tools []agenttool.Tool
	// live supplies the set per turn, for a source whose list changes,
	// such as a remote MCP server.
	live func(context.Context) []agenttool.Tool
}

func (s source) at(ctx context.Context) []agenttool.Tool {
	if s.live != nil {
		return s.live(ctx)
	}
	return s.tools
}

// toolSet unions the sources into one namespace, keeping the first tool
// to claim a name and reporting the rest.
type toolSet struct {
	sources []source
	// onConflict is told about a collision found at turn time, which is
	// the only point at which a remote's list can collide with a name
	// it did not collide with when New checked. It is called at most
	// once per name per resolution.
	onConflict func(Conflict)
	// filter keeps the tools it returns true for, and runs before the
	// duplicate check so that filtering one of two tools claiming a
	// name resolves the collision instead of reporting it.
	filter func(source string, t agenttool.Tool) bool

	// mu guards seen, which keeps a conflict from being reported on
	// every turn for as long as the two sources both offer the name.
	mu   sync.Mutex
	seen map[Conflict]bool
}

// resolve returns the union, in source order, and the conflicts found.
func (ts *toolSet) resolve(ctx context.Context) ([]agenttool.Tool, []Conflict) {
	var out []agenttool.Tool
	var conflicts []Conflict
	from := map[string]string{}
	for _, s := range ts.sources {
		for _, t := range s.at(ctx) {
			if ts.filter != nil && !ts.filter(s.name, t) {
				continue
			}
			name := t.Name()
			if prev, dup := from[name]; dup {
				conflicts = append(conflicts, Conflict{Name: name, Kept: prev, Dropped: s.name})
				continue
			}
			from[name] = s.name
			out = append(out, t)
		}
	}
	return out, conflicts
}

// provider is the per-turn tool list: the union, with a later duplicate
// dropped rather than offered, since the loop would otherwise dispatch
// whichever the set returned first.
func (ts *toolSet) provider() func(context.Context) []agenttool.Tool {
	return func(ctx context.Context) []agenttool.Tool {
		tools, conflicts := ts.resolve(ctx)
		if len(conflicts) > 0 && ts.onConflict != nil {
			ts.report(conflicts)
		}
		return tools
	}
}

func (ts *toolSet) report(conflicts []Conflict) {
	ts.mu.Lock()
	fresh := make([]Conflict, 0, len(conflicts))
	for _, c := range conflicts {
		if ts.seen == nil {
			ts.seen = map[Conflict]bool{}
		}
		if !ts.seen[c] {
			ts.seen[c] = true
			fresh = append(fresh, c)
		}
	}
	ts.mu.Unlock()
	for _, c := range fresh {
		ts.onConflict(c)
	}
}
