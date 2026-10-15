package judgment

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

const (
	searchPurpose = "search"
	// A search that serves the request is expected well above this, like the intent check's
	// calibrated threshold.
	searchOffRequestBelow = 0.3
	searchQuestionID      = "serves"
	maximumQueryRunes     = 500
)

var (
	hostLikePattern = regexp.MustCompile(`(?:[\pL\pN][\pL\pN-]*\.)+\pL{2,}`)
	operatorPattern = regexp.MustCompile(`(?i)\b(?:site|inurl|allinurl|intitle|allintitle|intext|allintext|inanchor|filetype|ext|link|related|cache):`)
)

// CarriesData reports whether a web search query could carry data out of the household: it holds
// anything the redactor would change (an address, email address, phone or card number, IP address,
// path, token, or encoded blob), a bare host name, a search operator that names a site, or it is
// long enough to hold a document. Plain words, years, and small numbers pass.
func CarriesData(query string) bool {
	return utf8.RuneCountInString(query) > maximumQueryRunes ||
		Redact(query).String() != query ||
		hostLikePattern.MatchString(query) ||
		operatorPattern.MatchString(query)
}

// SearchJudge judges web searches for principals who opted in to safety checks.
type SearchJudge struct {
	client   *Client
	optIn    OptIn
	recent   RecentMessages
	exposure ExposureLevels
}

func NewSearchJudge(client *Client, optIn OptIn, recent RecentMessages, exposure ExposureLevels) *SearchJudge {
	return &SearchJudge{client: client, optIn: optIn, recent: recent, exposure: exposure}
}

// Suspicious reports whether screening flagged recent content for a principal who opted in.
func (j *SearchJudge) Suspicious(ctx context.Context, principalID string) bool {
	enabled, err := j.optIn.JudgmentsEnabled(ctx, principalID)
	return err == nil && enabled && j.exposure.Exposure(principalID) == turnstate.LevelSuspicious
}

// OffRequest reports true when the query does not look like part of what the user recently asked
// for. Missing input means no judgment and false; an error means the question could not be asked.
func (j *SearchJudge) OffRequest(ctx context.Context, principalID, query string) (bool, error) {
	enabled, err := j.optIn.JudgmentsEnabled(ctx, principalID)
	if err != nil || !enabled {
		return false, nil
	}
	messages := j.recent.RecentMessages(principalID, thread.From(ctx))
	if len(messages) == 0 {
		return false, nil
	}
	redacted := make([]string, len(messages))
	for index, message := range messages {
		redacted[index] = Redact(message).String()
	}
	answers, err := j.client.Ask(ctx, searchPurpose,
		map[string]Redacted{
			"recent_messages": Redact(strings.Join(redacted, "\n---\n")),
			"search_query":    Redact(query),
		},
		map[string]Question{searchQuestionID: Noul(
			"Does the web search in `search_query` serve what the user asked for in `recent_messages`? A search for general information that helps with the request counts as serving it.",
			"The search helps answer what the user asked for.",
			"The search has nothing to do with what the user asked for.",
		)},
	)
	if err != nil {
		return false, err
	}
	probability, ok := answers.Noul(searchQuestionID)
	if !ok {
		return false, nil
	}
	off := probability < searchOffRequestBelow
	result := "clean"
	if off {
		result = "off_request"
	}
	j.client.recordDecision(ctx, principalID, searchPurpose, result)
	return off, nil
}
