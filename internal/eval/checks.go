package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

const excerptRunes = 120

// Failure is one check that did not hold. Messages are short and name the check; they carry at most
// a capped excerpt of what was observed.
type Failure struct {
	// Step is the 1-based step the check belongs to; the runner sets it.
	Step    int    `json:"step,omitempty"`
	Check   string `json:"check"`
	Message string `json:"message"`
}

// CheckEnv is what the checks read besides the turn itself.
type CheckEnv struct {
	// Store is the world's store, read by the store checks.
	Store *sqlite.Store
	// Location is the case clock's zone, which local-time arguments and job times are read in.
	Location *time.Location
}

// CheckStep evaluates a step's expectations against what the step produced and returns every
// check that failed, in a fixed order.
func CheckStep(ctx context.Context, expect *Expect, record TurnRecord, env CheckEnv) []Failure {
	if expect == nil {
		return nil
	}
	if env.Location == nil {
		env.Location = time.UTC
	}
	var failures []Failure
	fail := func(check, format string, args ...any) {
		failures = append(failures, Failure{Check: check, Message: fmt.Sprintf(format, args...)})
	}
	checkCalls(expect, record, env, fail)
	for _, tool := range expect.NotCalled {
		if n := countCalls(record.ToolCalls, tool); n > 0 {
			fail("not_called", "%s was called %d time(s), expected none", tool, n)
		}
	}
	if expect.MaxCalls != nil && len(record.ToolCalls) > *expect.MaxCalls {
		fail("max_calls", "%d tool calls, at most %d allowed", len(record.ToolCalls), *expect.MaxCalls)
	}
	if p := expect.Proposals; p != nil {
		checkProposals(p, record, fail)
	}
	if expect.NoWrites {
		checkNoWrites(record, fail)
	}
	if r := expect.Reply; r != nil {
		checkReply(r, record, fail)
	}
	for i, raw := range expect.Store {
		name, err := storeCheckName(raw)
		if err != nil {
			fail("store", "store[%d]: %v", i, err)
			continue
		}
		evaluate, ok := storeEvaluators[name]
		switch {
		case !ok:
			fail("store."+name, "no evaluator for this store check")
		case env.Store == nil:
			fail("store."+name, "no store to read")
		default:
			if err := evaluate(ctx, raw, env); err != nil {
				fail("store."+name, "%v", err)
			}
		}
	}
	return failures
}

func countCalls(calls []ToolCall, tool string) int {
	n := 0
	for _, c := range calls {
		if c.Name == tool {
			n++
		}
	}
	return n
}

func checkCalls(expect *Expect, record TurnRecord, env CheckEnv, fail func(check, format string, args ...any)) {
	position := -1
	for i, want := range expect.Calls {
		var matched []int
		reason := ""
		for j, got := range record.ToolCalls {
			if got.Name != want.Tool {
				continue
			}
			ok, why := matchArgs(got, want.Args, env.Location)
			switch {
			case !ok:
				reason = firstNonEmpty(reason, why)
			case expect.Ordered && j <= position:
				reason = firstNonEmpty(reason, "only before the previous expected call")
			default:
				matched = append(matched, j)
			}
		}
		least := 1
		if want.Min != nil {
			least = *want.Min
		}
		switch {
		case len(matched) < least:
			if reason == "" && countCalls(record.ToolCalls, want.Tool) == 0 {
				reason = "never called"
			}
			message := fmt.Sprintf("calls[%d] %s: want at least %d matching call(s), got %d", i, want.Tool, least, len(matched))
			if reason != "" {
				message += " (" + reason + ")"
			}
			fail("calls", "%s", message)
		case want.Max != nil && len(matched) > *want.Max:
			fail("calls", "calls[%d] %s: want at most %d matching call(s), got %d", i, want.Tool, *want.Max, len(matched))
		}
		if expect.Ordered && len(matched) > 0 {
			position = matched[min(max(least, 1), len(matched))-1]
		}
	}
}

func firstNonEmpty(current, next string) string {
	if current != "" {
		return current
	}
	return next
}

func matchArgs(call ToolCall, exprs []ArgExpr, location *time.Location) (bool, string) {
	if len(exprs) == 0 {
		return true, ""
	}
	var document any
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &document); err != nil {
			return false, "arguments are not JSON"
		}
	}
	for _, expr := range exprs {
		value, found := lookupPath(document, expr.Path)
		if !found {
			return false, fmt.Sprintf("args.%s missing", expr.Path)
		}
		if ok, why := matchValue(value, expr, location); !ok {
			return false, fmt.Sprintf("args.%s = %s %s", expr.Path, excerpt(renderValue(value)), why)
		}
	}
	return true, ""
}

func matchValue(value any, expr ArgExpr, location *time.Location) (bool, string) {
	switch {
	case len(expr.Equals) > 0:
		var want any
		if err := json.Unmarshal(expr.Equals, &want); err != nil {
			return false, "has an unreadable expected value"
		}
		if !reflect.DeepEqual(value, want) {
			return false, "is not the expected value " + excerpt(string(expr.Equals))
		}
	case expr.Contains != nil:
		if !strings.Contains(strings.ToLower(renderValue(value)), strings.ToLower(*expr.Contains)) {
			return false, "does not contain " + strconv.Quote(excerpt(*expr.Contains))
		}
	case expr.Matches != nil:
		re, err := regexp.Compile(*expr.Matches)
		if err != nil {
			return false, "has an invalid pattern"
		}
		if !re.MatchString(renderValue(value)) {
			return false, "does not match " + strconv.Quote(excerpt(*expr.Matches))
		}
	case expr.Within != nil:
		text, ok := value.(string)
		if !ok {
			return false, "is not a time"
		}
		if ok, why := withinTime(text, *expr.Within, location); !ok {
			return false, why
		}
	}
	return true, ""
}

func renderValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// lookupPath reads a dot path: object keys and array indexes, such as "events.0.start".
func lookupPath(document any, path string) (any, bool) {
	current := document
	for _, part := range strings.Split(path, ".") {
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[part]
			if !ok {
				return nil, false
			}
			current = next
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

var argTimeLayouts = []string{
	ClockLayout, "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02 15:04:05",
	time.RFC3339, timeOfDayLayout, "15:04:05",
}

// withinTime reads a local time argument and checks it against the bounds: whole date-times, or
// times of day, which wrap past midnight when the start is after the end. A value with an offset is
// read in the case's zone.
func withinTime(value string, bounds Within, location *time.Location) (bool, string) {
	var at time.Time
	var dated bool
	parsed := false
	for _, layout := range argTimeLayouts {
		t, err := time.ParseInLocation(layout, strings.TrimSpace(value), location)
		if err != nil {
			continue
		}
		at, parsed = t.In(location), true
		dated = layout != timeOfDayLayout && layout != "15:04:05"
		break
	}
	if !parsed {
		return false, "is not a local time"
	}
	if from, err := time.ParseInLocation(ClockLayout, bounds.From, location); err == nil {
		to, _ := time.ParseInLocation(ClockLayout, bounds.To, location)
		if !dated {
			return false, "has no date"
		}
		if at.Before(from) || at.After(to) {
			return false, fmt.Sprintf("is outside %s..%s", bounds.From, bounds.To)
		}
		return true, ""
	}
	from, errFrom := time.Parse(timeOfDayLayout, bounds.From)
	to, errTo := time.Parse(timeOfDayLayout, bounds.To)
	if errFrom != nil || errTo != nil {
		return false, "has unreadable bounds"
	}
	minutes := func(t time.Time) int { return t.Hour()*60 + t.Minute() }
	m, lo, hi := minutes(at), minutes(from), minutes(to)
	inside := m >= lo && m <= hi
	if lo > hi {
		inside = m >= lo || m <= hi
	}
	if !inside {
		return false, fmt.Sprintf("is outside %s..%s", bounds.From, bounds.To)
	}
	return true, ""
}

func checkProposals(p *Proposals, record TurnRecord, fail func(check, format string, args ...any)) {
	n := len(record.Proposals)
	if n < p.Min {
		fail("proposals", "%d proposal(s), at least %d expected", n, p.Min)
	}
	if p.Max != nil && n > *p.Max {
		fail("proposals", "%d proposal(s), at most %d expected", n, *p.Max)
	}
	for _, tool := range p.Tools {
		found := false
		for _, proposal := range record.Proposals {
			found = found || proposal.Tool == tool
		}
		if !found {
			fail("proposals", "no proposal of %s", tool)
		}
	}
}

func checkNoWrites(record TurnRecord, fail func(check, format string, args ...any)) {
	if n := len(record.Proposals); n > 0 {
		fail("no_writes", "%d proposal(s) offered, expected no writes", n)
	}
	for _, proposal := range record.Resolved {
		switch proposal.Status {
		case approval.StatusConfirmed, approval.StatusExecuted, approval.StatusDone:
			fail("no_writes", "%s was confirmed, expected no writes", proposal.Tool)
		}
	}
}

func checkReply(r *ReplyExpect, record TurnRecord, fail func(check, format string, args ...any)) {
	if r.Quiet {
		if !record.Quiet {
			fail("reply.quiet", "expected a quiet run, got a reply %q", excerpt(record.Reply))
		}
		return
	}
	reply := record.Reply
	lowered := strings.ToLower(reply)
	for _, text := range r.Contains {
		if !strings.Contains(lowered, strings.ToLower(text)) {
			fail("reply.contains", "reply lacks %q; reply starts %q", excerpt(text), excerpt(reply))
		}
	}
	for _, text := range r.NotContains {
		if strings.Contains(lowered, strings.ToLower(text)) {
			fail("reply.not_contains", "reply contains %q; reply starts %q", excerpt(text), excerpt(reply))
		}
	}
	for _, expr := range r.Matches {
		if re, err := regexp.Compile(expr); err != nil || !re.MatchString(reply) {
			fail("reply.matches", "reply does not match %q; reply starts %q", excerpt(expr), excerpt(reply))
		}
	}
	if r.Language != "" {
		if err := checkLanguage(r.Language, reply); err != nil {
			fail("reply.language", "%v; reply starts %q", err, excerpt(reply))
		}
	}
}

// cyrillicLanguages are the languages whose script the detector can tell from Latin.
var cyrillicLanguages = map[string]bool{"ru": true, "uk": true, "be": true, "bg": true, "sr": true, "mk": true}

// checkLanguage decides by script alone: a Cyrillic language needs most of the reply's letters
// to be Cyrillic, any other language needs most of them to be Latin. It cannot tell English from
// German or French, only Cyrillic from Latin text, and it ignores letters of other scripts.
func checkLanguage(tag, text string) error {
	primary, _, _ := strings.Cut(strings.ToLower(tag), "-")
	var cyrillic, latin int
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyrillic++
		case unicode.Is(unicode.Latin, r):
			latin++
		}
	}
	if cyrillic+latin == 0 {
		return fmt.Errorf("reply has no Cyrillic or Latin letters to judge %s by", tag)
	}
	share := float64(cyrillic) / float64(cyrillic+latin)
	if cyrillicLanguages[primary] {
		if share <= 0.5 {
			return fmt.Errorf("reply is %.0f%% Cyrillic, expected %s", share*100, tag)
		}
		return nil
	}
	if share > 0.5 {
		return fmt.Errorf("reply is %.0f%% Cyrillic, expected %s", share*100, tag)
	}
	return nil
}

// excerpt collapses whitespace and caps the text, so a failure never carries a whole reply.
func excerpt(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= excerptRunes {
		return text
	}
	return string([]rune(text)[:excerptRunes]) + "…"
}
