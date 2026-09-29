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

// ToolOrigin is one tool in the union and the source it came from: the
// label [WithToolFilter], [WithToolWrap] and a [Conflict] use, such as
// "WithTools", "WithSkills", "WithMemory", "WithChildAgent #1 explore"
// or "mcp:#1 some-server". [Kit.Tools] lists them.
type ToolOrigin struct {
	Name   string
	Source string
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
	// own is the kit's wrapper for this source's tools, applied after
	// the product's; nil for none. The skill grant is the one there is.
	own func(agenttool.Tool) agenttool.Tool

	// wrapped holds tools wrapped once, in [toolSet.prepare], for a
	// fixed source; it is nil for a live one, which is wrapped each
	// turn.
	wrapped []agenttool.Tool
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
	// name resolves the collision instead of reporting it. It sees the
	// tool its source produced, before any wrapper.
	filter func(source string, t agenttool.Tool) bool
	// wrap is the product's wrapper, applied after the filter and
	// before the source's own.
	wrap func(source string, t agenttool.Tool) agenttool.Tool

	// mu guards seen, which keeps a conflict from being reported on
	// every turn for as long as the two sources both offer the name.
	mu   sync.Mutex
	seen map[Conflict]bool
}

// wrapOne applies the product's wrapper and then the source's own. A
// nil from the product's drops the tool.
func (ts *toolSet) wrapOne(s *source, t agenttool.Tool) agenttool.Tool {
	if ts.wrap != nil {
		if t = ts.wrap(s.name, t); t == nil {
			return nil
		}
	}
	if s.own != nil {
		t = s.own(t)
	}
	return t
}

// prepare wraps each fixed source's tools once, so a wrapper over a
// tool that never changes is not rebuilt every turn.
func (ts *toolSet) prepare() {
	for i := range ts.sources {
		s := &ts.sources[i]
		if s.live != nil {
			continue
		}
		s.wrapped = make([]agenttool.Tool, len(s.tools))
		for j, t := range s.tools {
			s.wrapped[j] = ts.wrapOne(s, t)
		}
	}
}

// resolve returns the union, in source order, where each tool came
// from, and the conflicts found.
func (ts *toolSet) resolve(ctx context.Context) ([]agenttool.Tool, []ToolOrigin, []Conflict) {
	var out []agenttool.Tool
	var origins []ToolOrigin
	var conflicts []Conflict
	from := map[string]string{}
	for i := range ts.sources {
		s := &ts.sources[i]
		raw, wrapped := s.tools, s.wrapped
		if s.live != nil {
			raw, wrapped = s.live(ctx), nil
		}
		for j, t := range raw {
			if ts.filter != nil && !ts.filter(s.name, t) {
				continue
			}
			if wrapped != nil {
				t = wrapped[j]
			} else {
				t = ts.wrapOne(s, t)
			}
			if t == nil {
				continue
			}
			name := t.Name()
			if prev, dup := from[name]; dup {
				conflicts = append(conflicts, Conflict{Name: name, Kept: prev, Dropped: s.name})
				continue
			}
			from[name] = s.name
			out = append(out, t)
			origins = append(origins, ToolOrigin{Name: name, Source: s.name})
		}
	}
	return out, origins, conflicts
}

// provider is the per-turn tool list: the union, with a later duplicate
// dropped rather than offered, since the loop would otherwise dispatch
// whichever the set returned first.
func (ts *toolSet) provider() func(context.Context) []agenttool.Tool {
	return func(ctx context.Context) []agenttool.Tool {
		tools, _, conflicts := ts.resolve(ctx)
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
