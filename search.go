package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// ConversationSearchTool is the name of the tool [WithSessionSearch]
// offers.
const ConversationSearchTool = "conversation_search"

// Defaults for conversation_search.
const (
	// DefaultSearchSessions is how many sessions a search reads, newest
	// first, when [SearchFilter] gives no Limit: a scan reads each one
	// whole, so an unbounded search costs as much as the store is large.
	DefaultSearchSessions = 100
	// DefaultSearchHits is how many messages a search returns when the
	// call names no limit. A result stays in the transcript for the rest
	// of the conversation, so it is low.
	DefaultSearchHits = 10
	// MaxSearchHits is the most messages one call returns, whatever
	// limit it names.
	MaxSearchHits = 50
)

// SearchOption configures conversation_search, for [WithSessionSearch]
// and [SessionSearchTool].
type SearchOption func(*searchSettings)

type searchSettings struct {
	filter       agentsession.ListFilter
	own          bool
	omitRedacted bool
	hits         int
}

// SearchFilter selects the sessions searched, as it selects what a
// store's List gives: a project's by CWD, a harness's own, the top level
// alone. Its Limit caps the sessions read, newest first; zero is
// [DefaultSearchSessions], and a negative Limit reads every session the
// filter keeps. WithNames does not change a search.
func SearchFilter(f agentsession.ListFilter) SearchOption {
	return func(o *searchSettings) { o.filter = f }
}

// SearchOwnSession searches the calling conversation's own session too,
// its whole path, so a message a fold took out of the model's context
// is found again. By default the search leaves it out, since what the
// model has in context it need not search for.
func SearchOwnSession() SearchOption {
	return func(o *searchSettings) { o.own = true }
}

// SearchOmitRedacted leaves out the redacted sessions a store holds
// beside their originals, so a message the redaction kept is found once,
// in its original ([agentsession.Query].OmitRedacted). A store that holds
// redacted sessions alone finds nothing under it.
func SearchOmitRedacted() SearchOption {
	return func(o *searchSettings) { o.omitRedacted = true }
}

// SearchLimit sets how many messages a call that names no limit
// returns; the default is [DefaultSearchHits], and a value under one
// means the default. No call returns more than [MaxSearchHits].
func SearchLimit(n int) SearchOption {
	return func(o *searchSettings) { o.hits = n }
}

// WithSessionSearch offers conversation_search, [SessionSearchTool]
// over the recorder of each run, the one [ContextWithRecorder] put on
// its context or the kit's own: the model finds what an earlier
// conversation said, the store's other sessions searched through
// agentsession's [agentsession.Search] (RFC 0002, Search), which a
// store answers itself when it is an [agentsession.Searcher] and
// otherwise scans. The tool joins the set after the memory tools, under
// the source label "WithSessionSearch"; the policy decides it as any
// other tool, and the kit does not allow it. A run with no recorder, a
// kit with no session whose front put none on the context, has no store
// to search, and the call fails saying so.
func WithSessionSearch(opts ...SearchOption) Option {
	return func(s *settings) {
		s.search = true
		s.searchOpts = append(s.searchOpts, opts...)
	}
}

// sessionSearchArgs are conversation_search's arguments.
type sessionSearchArgs struct {
	Query     string   `json:"query" desc:"Words that must all appear in a message, in any case"`
	Roles     []string `json:"roles,omitempty" desc:"Only messages of these roles: user, assistant, system, developer; every role when omitted"`
	Limit     int      `json:"limit,omitempty" desc:"At most this many messages; keep it low, results stay in the conversation"`
	StartDate string   `json:"start_date,omitempty" desc:"Only messages from this day on (YYYY-MM-DD, UTC) or this instant on (RFC 3339)"`
	EndDate   string   `json:"end_date,omitempty" desc:"Only messages up to the end of this day (YYYY-MM-DD, UTC) or up to this instant (RFC 3339)"`
}

// SessionSearchTool returns conversation_search over the store of the
// recorder recorderFor gives for a call's context, which is
// [WithSessionSearch]'s tool, built from exported calls, for a product
// that offers it without the kit: recorderFor is the run's recorder, as
// docs/manual.md writes it. A call searches that recorder's store with
// [agentsession.Search] for the messages holding every word of its
// query, newest sessions first:
//
//   - the session the call's record goes to, [session.Recorder.SessionOf],
//     is left out unless [SearchOwnSession] is given;
//   - [SearchFilter] selects the sessions and caps how many are read;
//   - roles keeps messages of those roles; start_date and end_date bound
//     each message's time, both inclusive: a day is a UTC day and an
//     RFC 3339 time an instant, turned into the exclusive bounds
//     agentsession's Query takes;
//   - limit caps the messages, [DefaultSearchHits] when the call names
//     none and never more than [MaxSearchHits].
//
// Each hit is a line naming its session, the session's name when it has
// one, the message's time and role, and then its snippet, the run of
// text agentsession cut around the first word found, its white space
// collapsed. The tool states its flow as reading untrusted content
// (agenttool RFC 0001, flow), since what it returns is what earlier
// models, tools and pages wrote, and a policy that guards against
// injection treats its output so. It is read-only and safe to replay.
func SessionSearchTool(recorderFor func(context.Context) *session.Recorder, opts ...SearchOption) agenttool.Tool {
	var o searchSettings
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.hits < 1 {
		o.hits = DefaultSearchHits
	}
	switch {
	case o.filter.Limit == 0:
		o.filter.Limit = DefaultSearchSessions
	case o.filter.Limit < 0:
		o.filter.Limit = 0
	}
	return agenttool.New(ConversationSearchTool,
		"Search earlier conversations for messages holding every word of a query, newest conversations first. "+
			"Use it to find what was said or decided in a conversation that is not this one. "+
			"Each result names its conversation, when the message was written and by whom, and a snippet of it.",
		func(ctx context.Context, args sessionSearchArgs) (string, error) {
			return o.search(ctx, recorderFor, args)
		},
		agenttool.WithAnnotations(agenttool.Annotations{Title: "Search conversations", ReadOnly: true}),
		agenttool.WithReplay(func(context.Context, json.RawMessage) agenttool.Replay { return agenttool.ReplaySafe }),
		agenttool.WithFlow(agenttool.Flow{Reads: []agenttool.Label{agenttool.LabelUntrusted}}),
	)
}

// errNoSearchStore is the call's error when no recorder is in reach.
var errNoSearchStore = errors.New("there is no session store to search in this conversation")

// search runs one call.
func (o searchSettings) search(ctx context.Context, recorderFor func(context.Context) *session.Recorder, args sessionSearchArgs) (string, error) {
	rec := recorderFor(ctx)
	if rec == nil || rec.Store() == nil {
		return "", errNoSearchStore
	}
	q := agentsession.Query{Text: args.Query, Filter: o.filter, OmitRedacted: o.omitRedacted, Limit: o.hits}
	if !o.own {
		q.Exclude = []string{rec.SessionOf(ctx)}
	}
	if args.Limit > 0 {
		q.Limit = min(args.Limit, MaxSearchHits)
	}
	for _, r := range args.Roles {
		role := openresponses.Role(strings.ToLower(strings.TrimSpace(r)))
		switch role {
		case openresponses.RoleUser, openresponses.RoleAssistant, openresponses.RoleSystem, openresponses.RoleDeveloper:
		default:
			return "", fmt.Errorf("role %q is not one of user, assistant, system, developer", r)
		}
		if !slices.Contains(q.Roles, role) {
			q.Roles = append(q.Roles, role)
		}
	}
	if args.StartDate != "" {
		from, _, err := searchTime(args.StartDate)
		if err != nil {
			return "", fmt.Errorf("start_date: %w", err)
		}
		// Inclusive: Query's After is exclusive.
		q.After = from.Add(-time.Nanosecond)
	}
	if args.EndDate != "" {
		to, day, err := searchTime(args.EndDate)
		if err != nil {
			return "", fmt.Errorf("end_date: %w", err)
		}
		// Inclusive, to the end of a day: Query's Before is exclusive.
		if day {
			q.Before = to.AddDate(0, 0, 1)
		} else {
			q.Before = to.Add(time.Nanosecond)
		}
	}
	if !q.After.IsZero() && !q.Before.IsZero() && !q.After.Add(time.Nanosecond).Before(q.Before) {
		return "", errors.New("start_date is after end_date")
	}
	var b strings.Builder
	n := 0
	for h, err := range agentsession.Search(ctx, rec.Store(), q) {
		if err != nil {
			return "", fmt.Errorf("search: %w", err)
		}
		n++
		fmt.Fprintf(&b, "\n- session %s", h.Session)
		if h.Name != "" {
			fmt.Fprintf(&b, " %q", h.Name)
		}
		fmt.Fprintf(&b, ", %s, %s: %s", h.Time.UTC().Format(time.RFC3339), h.Role, strings.Join(strings.Fields(h.Snippet), " "))
	}
	if n == 0 {
		return "No message in another conversation matched.", nil
	}
	return fmt.Sprintf("%d message(s) matched, newest conversations first:", n) + b.String(), nil
}

// searchTime reads a date, YYYY-MM-DD as the start of that UTC day, or
// an RFC 3339 time, and reports which it was.
func searchTime(v string) (t time.Time, day bool, err error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, true, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, false, nil
	}
	return time.Time{}, false, fmt.Errorf("%q is neither a date (YYYY-MM-DD) nor an RFC 3339 time", v)
}
