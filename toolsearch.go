package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
)

// ToolSearchTool is the name of the tool [WithToolSearch] offers, which
// loads a deferred tool.
const ToolSearchTool = "tool_search"

// PartToolSearch is the part [WithToolSearch] lists the deferred tools
// in, by name and a one-line summary, with [SourceToolSearch] as its
// source. [WithOrder] places it by this ID, and an order that leaves it
// out puts it right after [PartMCP]: like the skill catalogue and the
// servers' instructions it is an account of what the agent can reach,
// and it changes only when the deferred tools do, so it sits early,
// inside the prefix a provider caches.
const PartToolSearch = "tool_search"

// SourceToolSearch is the source of [PartToolSearch]: the kit writes the
// listing, from the tools' own names and descriptions.
const SourceToolSearch = "agentkit"

// Limits of tool_search.
const (
	// DefaultToolSearchResults is how many tools a keyword search loads
	// when the call names no max_results.
	DefaultToolSearchResults = 5
	// MaxToolSearchResults is the most tools one keyword search loads,
	// whatever max_results it names. A select: call loads every name it
	// gives.
	MaxToolSearchResults = 20
	// ToolSummaryRunes caps a deferred tool's summary in the listing:
	// the first sentence of its description, white space collapsed, cut
	// to this many runes.
	ToolSummaryRunes = 120
)

// ToolSearch holds tools back from the model until it asks for them: a
// deferred tool is listed by name and summary in [PartToolSearch], and
// its definition is sent only once tool_search has loaded it. It keeps,
// per conversation, the names loaded, in the order they were loaded.
//
// [WithToolSearch] builds one over the kit's tool set, and
// [Kit.ToolSearch] returns it. It is exported so the manual path is
// calls and not a copy (docs/manual.md): a product without the kit
// builds one with [NewToolSearch], offers [ToolSearch.Offer]'s list from
// its ToolProvider and renders [ToolSearch.Part] into its instructions.
//
// A loaded tool joins the list from the next request on, after the tools
// already offered and after the tools loaded before it, and stays for
// the rest of the conversation, so a request's tools are the previous
// request's with the new ones appended and the prefix a provider caches
// holds. A conversation's recorder then writes the change as a config
// entry's tools_added, as for any tool list that grew, which is the
// record of when each tool entered the context; replay rebuilds the same
// requests from it, and a conversation resumed from it starts with the
// same tools loaded ([ToolSearchSeed]).
//
// Loading changes what the model is offered and nothing it may do: a
// deferred tool is still its source's tool, decided by the policy when
// it is called. Every method is safe for concurrent use.
type ToolSearch struct {
	held  func(context.Context) []agenttool.Tool
	scope func(context.Context) string
	seed  func(context.Context) []string
	tool  agenttool.Tool

	mu sync.Mutex
	// loaded is each conversation's loaded names, by its scope, in load
	// order. A conversation is present once seeded, even with none
	// loaded. Past the bound the conversation used least recently is
	// forgotten, and seeded again from its record when it comes back.
	loaded bounded[[]string]
}

// ToolSearchOption configures [NewToolSearch].
type ToolSearchOption func(*ToolSearch)

// ToolSearchSeed gives the names a conversation the [ToolSearch] has
// not seen yet starts with loaded: for a resumed conversation, the tools
// its record has in force at the leaf, so its first request offers what
// its last one did. seed is called once per conversation, before its
// first list or load, with that conversation's context; a name that is
// not a held tool is kept and offers nothing. [WithToolSearch] seeds from
// the session the run records into, [ToolsInForce] of its path.
func ToolSearchSeed(seed func(context.Context) []string) ToolSearchOption {
	return func(s *ToolSearch) { s.seed = seed }
}

// NewToolSearch returns a ToolSearch over held, the deferred tools the
// conversation on a context may load, in the order they are listed, and
// scope, which names the conversation a context belongs to; nil puts
// every context in one. [WithToolSearch] gives it the deferred tools of
// the kit's union after the policy's offer filter, and [Kit.GrantScope].
func NewToolSearch(held func(context.Context) []agenttool.Tool, scope func(context.Context) string, opts ...ToolSearchOption) *ToolSearch {
	s := &ToolSearch{held: held, scope: scope}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	s.tool = agenttool.New(ToolSearchTool,
		"Load tools that are listed in your instructions as loaded on request, so you can call them. "+
			`Pass "select:" and exact names, comma-separated ("select:name_a,name_b"), or keywords to search the deferred tools' names and descriptions. `+
			"A loaded tool can be called from your next step on and stays loaded for the rest of the conversation.",
		func(ctx context.Context, args toolSearchArgs) (string, error) { return s.search(ctx, args) },
		agenttool.WithAnnotations(agenttool.Annotations{Title: "Load tools", ReadOnly: true, Idempotent: true}),
		agenttool.WithReplay(func(context.Context, json.RawMessage) agenttool.Replay { return agenttool.ReplaySafe }),
	)
	return s
}

// toolSearchArgs are tool_search's arguments.
type toolSearchArgs struct {
	Query      string `json:"query" desc:"\"select:\" and exact tool names, comma-separated, or keywords to search the deferred tools' names and descriptions"`
	MaxResults int    `json:"max_results,omitempty" desc:"At most this many tools for a keyword search; 5 when omitted"`
}

// Tool is tool_search, which [ToolSearch.Offer] puts in the list while
// any tool is held.
func (s *ToolSearch) Tool() agenttool.Tool { return s.tool }

func (s *ToolSearch) scopeOf(ctx context.Context) string {
	if s.scope == nil {
		return ""
	}
	return s.scope(ctx)
}

// names is the conversation's loaded names, seeding it first when the
// ToolSearch has not seen it. The seed is read off the lock, since it
// may read a store.
func (s *ToolSearch) names(ctx context.Context) (string, []string) {
	key := s.scopeOf(ctx)
	s.mu.Lock()
	got, ok := s.loaded.get(key)
	s.mu.Unlock()
	if ok {
		return key, got
	}
	var seeded []string
	if s.seed != nil {
		for _, n := range s.seed(ctx) {
			if n != "" && !slices.Contains(seeded, n) {
				seeded = append(seeded, n)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if got, ok := s.loaded.get(key); ok {
		// Seeded meanwhile by another call of the conversation's.
		return key, got
	}
	s.loaded.put(key, seeded)
	return key, seeded
}

// Load marks names loaded in the conversation on ctx, after those loaded
// already, in the order given, and returns the ones that were not loaded
// before. It does not check that a name is a held tool: tool_search
// does, and a product may load a tool ahead of the model.
func (s *ToolSearch) Load(ctx context.Context, names ...string) []string {
	key, _ := s.names(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, _ := s.loaded.get(key)
	var added []string
	next := slices.Clone(cur)
	for _, n := range names {
		if n == "" || slices.Contains(next, n) {
			continue
		}
		next = append(next, n)
		added = append(added, n)
	}
	if len(added) > 0 {
		s.loaded.put(key, next)
	}
	return added
}

// Loaded is the held tools the conversation on ctx has loaded, by name,
// in load order: a name seeded from the record that is not a held tool
// now, one offered without deferral then, is not among them.
func (s *ToolSearch) Loaded(ctx context.Context) []string {
	_, names := s.names(ctx)
	held := map[string]bool{}
	for _, t := range s.held(ctx) {
		held[t.Name()] = true
	}
	var out []string
	for _, n := range names {
		if held[n] {
			out = append(out, n)
		}
	}
	return out
}

// Offer is the list a request of the conversation on ctx is offered:
// offered, the tools not deferred, in their order; then tool_search, when
// any tool is held; then the held tools the conversation has loaded, in
// the order they were loaded. A loaded name that held does not have now,
// a server's tool while it restarts, offers nothing and keeps its place.
// A tool in offered named tool_search keeps the name, and the
// ToolSearch's tool is not offered.
func (s *ToolSearch) Offer(ctx context.Context, offered, held []agenttool.Tool) []agenttool.Tool {
	if len(held) == 0 {
		return offered
	}
	_, names := s.names(ctx)
	out := make([]agenttool.Tool, 0, len(offered)+1+len(names))
	out = append(out, offered...)
	if !slices.ContainsFunc(offered, func(t agenttool.Tool) bool { return t.Name() == ToolSearchTool }) {
		out = append(out, s.tool)
	}
	byName := make(map[string]agenttool.Tool, len(held))
	for _, t := range held {
		if _, dup := byName[t.Name()]; !dup {
			byName[t.Name()] = t
		}
	}
	for _, n := range names {
		if t, ok := byName[n]; ok {
			out = append(out, t)
		}
	}
	return out
}

// Part is [PartToolSearch] for the conversation on ctx, in the shape
// [WithPart] renders: the held tools, each by name and summary, in held's
// order, under a paragraph saying how to load them; empty, and so not
// joined, when nothing is held. Every held tool is listed, loaded or not,
// so the text changes only when the held tools do.
func (s *ToolSearch) Part(ctx context.Context) (PartRender, error) {
	return PartRender{Text: ToolSearchListing(s.held(ctx))}, nil
}

// ToolSearchListing is the text of [PartToolSearch] for held: a heading,
// how to load a tool, and a line per tool, its name and
// [ToolSummary]. It is empty for no tools.
func ToolSearchListing(held []agenttool.Tool) string {
	if len(held) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Tools loaded on request\n\n")
	b.WriteString("The tools below are available, but their definitions are not sent until you load them. ")
	b.WriteString(`To use one, call ` + ToolSearchTool + ` with "select:" and exact names, comma-separated ("select:name_a,name_b"), or with keywords to search their names and descriptions. `)
	b.WriteString("A loaded tool can be called from your next step on and stays loaded for the rest of the conversation.\n")
	for _, t := range held {
		b.WriteString("\n- ")
		b.WriteString(t.Name())
		if sum := ToolSummary(t.Description()); sum != "" {
			b.WriteString(": ")
			b.WriteString(sum)
		}
	}
	return b.String()
}

// ToolSummary is a description's first sentence, its white space
// collapsed, cut to [ToolSummaryRunes] runes with an ellipsis: the line a
// deferred tool is listed with. A description is often several
// paragraphs of usage; the listing says what the tool is for, and the
// definition the model loads says the rest.
func ToolSummary(description string) string {
	text := strings.Join(strings.Fields(description), " ")
	if i := strings.Index(text, ". "); i >= 0 {
		text = text[:i+1]
	}
	if utf8.RuneCountInString(text) <= ToolSummaryRunes {
		return text
	}
	r := []rune(text)
	return strings.TrimRight(string(r[:ToolSummaryRunes-1]), " ") + "…"
}

// errEmptyToolQuery is tool_search's error for a query with nothing in it.
var errEmptyToolQuery = errors.New(`the query is empty: pass "select:" and exact tool names, or keywords`)

// search runs one tool_search call: it finds the tools, loads them, and
// says which.
func (s *ToolSearch) search(ctx context.Context, args toolSearchArgs) (string, error) {
	q := strings.TrimSpace(args.Query)
	if q == "" {
		return "", errEmptyToolQuery
	}
	held := s.held(ctx)
	var (
		found   []agenttool.Tool
		unknown []string
	)
	if rest, ok := strings.CutPrefix(q, "select:"); ok {
		for _, n := range strings.Split(rest, ",") {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			i := slices.IndexFunc(held, func(t agenttool.Tool) bool { return t.Name() == n })
			switch {
			case i < 0:
				if !slices.Contains(unknown, n) {
					unknown = append(unknown, n)
				}
			case !slices.Contains(found, held[i]):
				found = append(found, held[i])
			}
		}
		if len(found) == 0 && len(unknown) == 0 {
			return "", errEmptyToolQuery
		}
	} else {
		limit := args.MaxResults
		if limit < 1 {
			limit = DefaultToolSearchResults
		}
		found = matchTools(held, q, min(limit, MaxToolSearchResults))
	}

	names := make([]string, len(found))
	for i, t := range found {
		names[i] = t.Name()
	}
	added := s.Load(ctx, names...)

	var b strings.Builder
	switch {
	case len(found) == 0 && len(unknown) == 0:
		fmt.Fprintf(&b, "No deferred tool matched %q. The tools you can load are listed in your instructions under \"Tools loaded on request\".", q)
	case len(found) > 0:
		fmt.Fprintf(&b, "%d tool(s) loaded; each can be called from your next step on:", len(found))
		for _, t := range found {
			b.WriteString("\n- ")
			b.WriteString(t.Name())
			if sum := ToolSummary(t.Description()); sum != "" {
				b.WriteString(": ")
				b.WriteString(sum)
			}
			if !slices.Contains(added, t.Name()) {
				b.WriteString(" (loaded already)")
			}
		}
	}
	if len(unknown) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "No deferred tool is named %s; the tools you can load are listed in your instructions under \"Tools loaded on request\".", strings.Join(quoteAll(unknown), ", "))
	}
	return b.String(), nil
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// matchTools is the keyword search: a tool named exactly as the query
// first; then each tool scored by the query's words, two for a word in
// its name and one for a word only in its description, case ignored,
// a word written +word required in the name; the best first, ties in
// held's order, at most limit, none that scored nothing.
func matchTools(held []agenttool.Tool, q string, limit int) []agenttool.Tool {
	for _, t := range held {
		if t.Name() == q {
			return []agenttool.Tool{t}
		}
	}
	type scored struct {
		t     agenttool.Tool
		score int
	}
	var hits []scored
	terms := strings.Fields(strings.ToLower(q))
	for _, t := range held {
		name, desc := strings.ToLower(t.Name()), strings.ToLower(t.Description())
		score := 0
		for _, term := range terms {
			required := strings.HasPrefix(term, "+")
			term = strings.TrimPrefix(term, "+")
			if term == "" {
				continue
			}
			switch {
			case strings.Contains(name, term):
				score += 2
			case required:
				score = -1
			case strings.Contains(desc, term):
				score++
			}
			if score < 0 {
				break
			}
		}
		if score > 0 {
			hits = append(hits, scored{t, score})
		}
	}
	slices.SortStableFunc(hits, func(a, b scored) int { return b.score - a.score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]agenttool.Tool, len(hits))
	for i, h := range hits {
		out[i] = h.t
	}
	return out
}

// ToolsInForce is the names of the tools the settings at the end of path
// have in force, in their order: what the conversation's next request
// would be offered had nothing changed, read by folding the path's
// config entries from its last compaction's checkpoint. [WithToolSearch]
// seeds a resumed conversation's loaded tools from it.
func ToolsInForce(path []agentsession.Entry) []string {
	var st agentsession.Settings
	for _, e := range path {
		switch e := e.(type) {
		case *agentsession.CompactionEntry:
			st = e.Config
		case *agentsession.ConfigEntry:
			st = st.Apply(e)
		}
	}
	out := make([]string, 0, len(st.Tools))
	for _, t := range st.Tools {
		if n := agentsession.ToolName(t); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// ToolSearch is the kit's [WithToolSearch] state, which a front reads to
// show the tools a conversation has loaded, [ToolSearch.Loaded], or
// loads one ahead of the model; nil without WithToolSearch.
func (k *Kit) ToolSearch() *ToolSearch { return k.search }

// splitHeld divides the union's tools, by their origins, into those
// offered and those [WithToolSearch] holds back, each in union order,
// asking about each source once, with its tools.
func (k *Kit) splitHeld(tools []agenttool.Tool, origins []ToolOrigin) (offered, held []agenttool.Tool) {
	bySource := map[string][]agenttool.Tool{}
	for i, t := range tools {
		bySource[origins[i].Source] = append(bySource[origins[i].Source], t)
	}
	decided := make(map[string]bool, len(bySource))
	for src, ts := range bySource {
		decided[src] = k.deferTool(src, ts)
	}
	for i, t := range tools {
		if decided[origins[i].Source] {
			held = append(held, t)
		} else {
			offered = append(offered, t)
		}
	}
	return offered, held
}

// heldTools is the tools the conversation on ctx may load: the union's
// held tools as it resolves now, through the policy's offer filter
// under the conversation's grant scope, so a tool a bare deny removes is
// neither listed nor loaded. Before the tool set exists, for the
// listing New renders, it is drawn from the product's tools and the
// servers', the sources there are then, through the filter and the
// wrapper; a held tool of a source built later, such as a provider's,
// joins the listing from the first request.
func (k *Kit) heldTools(ctx context.Context) []agenttool.Tool {
	var (
		tools   []agenttool.Tool
		origins []ToolOrigin
	)
	if k.tools != nil {
		tools, origins, _ = k.tools.resolve(ctx)
	} else {
		early := &toolSet{filter: k.toolFilter, wrap: k.toolWrap}
		if len(k.earlyTools) > 0 {
			early.sources = append(early.sources, source{name: "WithTools", tools: k.earlyTools})
		}
		for _, sv := range k.mcpServers() {
			early.sources = append(early.sources, k.remoteSource(sv.label, sv.remote))
		}
		tools, origins, _ = early.resolveSources(ctx, early.sources)
	}
	_, held := k.splitHeld(tools, origins)
	if k.engine == nil || len(held) == 0 {
		return held
	}
	return k.engine.ToolProvider(func(context.Context) []agenttool.Tool { return held })(agentpolicy.ContextWithGrantScope(ctx, k.GrantScope(ctx)))
}

// toolsInForce seeds a conversation the kit's [ToolSearch] has not seen:
// the tools in force at the leaf of the session the run on ctx records
// into, or none without one.
func (k *Kit) toolsInForce(ctx context.Context) []string {
	rec := k.recorderFor(ctx)
	if rec == nil {
		return nil
	}
	sess := k.sessionOf(ctx, rec, runSessionID(ctx, rec))
	if sess == nil {
		return nil
	}
	return ToolsInForce(sess.Path(sess.Leaf()))
}
