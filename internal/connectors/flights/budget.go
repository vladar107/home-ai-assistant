package flights

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// UnitsPerLookup is what one flight status request costs on AeroDataBox, a tier 2 endpoint.
	UnitsPerLookup = 2

	// The scopes of the unit counters: every lookup, the lookups users asked for through the
	// tool, and one user's; ScopeSpent is a period the marketplace said was used up.
	ScopeAll   = "all"
	ScopeTool  = "tool"
	ScopeSpent = "spent"
	scopeUser  = "user:"

	// requestSpacing keeps requests under the free plan's one a second and 1,000 an hour.
	requestSpacing = 2 * time.Second

	// quotaBodyBytes is how much of a refusal is read to tell a spent quota from a rate limit.
	quotaBodyBytes = 4 << 10
)

// remainingHeaders count the units left in the subscription's period: RapidAPI sends the second,
// as the spike saw, and AeroDataBox's own gateway the first.
var remainingHeaders = []string{"X-Api-Credits-Remaining", "X-Ratelimit-Api-Units-Remaining"}

// Purpose is who a lookup is for: a user's question through the tool, or the tracking of a trip's
// flight.
type Purpose string

const (
	PurposeTool     Purpose = "tool"
	PurposeTracking Purpose = "tracking"
)

// UnitLimit is the most units one scope may spend in a period.
type UnitLimit struct {
	Scope string
	Limit int
}

// Units keeps the units each scope spent per quota period; the SQLite store implements it.
type Units interface {
	// ReserveFlightUnits adds units to every limited scope of the period, in one transaction, only
	// when each stays within its limit and the period is not marked spent. It returns "" when it
	// added them, and otherwise the scope that refused, ScopeSpent for a spent period.
	ReserveFlightUnits(ctx context.Context, period string, units int, limits []UnitLimit) (string, error)
	FlightUnitsUsed(ctx context.Context, period, scope string) (int, error)
	MarkFlightPeriodSpent(ctx context.Context, period string, at time.Time) error
	FlightPeriodSpent(ctx context.Context, period string) (bool, error)
}

// Budget is the units the household may spend in one quota period, which starts at 00:00 UTC on
// the reset day of each month, the last day of a month too short for it. The tool and each user
// have a share of it, so questions cannot starve the tracking or one user the others.
type Budget struct {
	Monthly  int
	Tool     int
	User     int
	ResetDay int
}

// NewBudget shares the monthly units out as the plan sets them at the default of 340: the tool
// gets 100 of them and a user 50, and both scale with a larger budget.
func NewBudget(monthly, resetDay int) Budget {
	return Budget{Monthly: monthly, Tool: monthly * 10 / 34, User: monthly * 5 / 34, ResetDay: resetDay}
}

// Period returns the quota period around the instant: its start and the start of the next.
func (b Budget) Period(at time.Time) (time.Time, time.Time) {
	at = at.UTC()
	start := b.resetIn(at.Year(), at.Month())
	if at.Before(start) {
		start = b.resetIn(at.Year(), at.Month()-1)
	}
	return start, b.resetIn(start.Year(), start.Month()+1)
}

func (b Budget) resetIn(year int, month time.Month) time.Time {
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	return first.AddDate(0, 0, min(max(b.ResetDay, 1), last)-1)
}

func periodKey(start time.Time) string { return start.Format(time.DateOnly) }

func (b Budget) limits(purpose Purpose, principalID string) []UnitLimit {
	limits := []UnitLimit{{ScopeAll, b.Monthly}}
	if purpose == PurposeTool {
		limits = append(limits, UnitLimit{ScopeTool, b.Tool}, UnitLimit{scopeUser + principalID, b.User})
	}
	return limits
}

type requestKey struct{}

type requestInfo struct {
	purpose     Purpose
	principalID string
}

func withRequest(ctx context.Context, purpose Purpose, principalID string) context.Context {
	return context.WithValue(ctx, requestKey{}, requestInfo{purpose: purpose, principalID: principalID})
}

func requestFrom(ctx context.Context) requestInfo {
	info, _ := ctx.Value(requestKey{}).(requestInfo)
	if info.purpose == "" {
		info.purpose = PurposeTracking
	}
	return info
}

// limiter spaces requests apart. It holds its lock while it waits, so requests leave one by one.
type limiter struct {
	mu    sync.Mutex
	next  time.Time
	gap   time.Duration
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if wait := l.next.Sub(l.now()); wait > 0 {
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
	}
	l.next = l.now().Add(l.gap)
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// meteredTransport reserves each attempt's units before it is sent, retries included since it
// sits under the retrying transport, waits its turn at the limiter, and reads the marketplace's
// own word on the quota from the answer.
type meteredTransport struct {
	next    http.RoundTripper
	units   Units
	budget  Budget
	limiter *limiter
	now     func() time.Time
	// used hears the period's units after each reservation; nil hears nothing.
	used func(ctx context.Context, used int)
}

func (t *meteredTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := request.Context()
	info := requestFrom(ctx)
	start, _ := t.budget.Period(t.now())
	period := periodKey(start)
	refused, err := t.units.ReserveFlightUnits(ctx, period, UnitsPerLookup, t.budget.limits(info.purpose, info.principalID))
	if err != nil {
		return nil, &budgetError{err: err}
	}
	if refused != "" {
		return nil, &spentError{scope: refused}
	}
	if t.used != nil {
		if used, err := t.units.FlightUnitsUsed(ctx, period, ScopeAll); err == nil {
			t.used(ctx, used)
		}
	}
	if err := t.limiter.wait(ctx); err != nil {
		return nil, err
	}
	response, err := t.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	// AeroDataBox answers 402 once the plan's units are spent.
	if response.StatusCode == http.StatusPaymentRequired || response.StatusCode == http.StatusTooManyRequests && quotaRefusal(response) {
		_ = response.Body.Close()
		return nil, t.spend(ctx, period)
	}
	for _, name := range remainingHeaders {
		value := response.Header.Get(name)
		if value == "" {
			continue
		}
		if remaining, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && remaining < UnitsPerLookup {
			// The answer still holds the flight; only the lookups after it are stopped.
			_ = t.units.MarkFlightPeriodSpent(ctx, period, t.now())
		}
	}
	return response, nil
}

func (t *meteredTransport) spend(ctx context.Context, period string) error {
	if err := t.units.MarkFlightPeriodSpent(ctx, period, t.now()); err != nil {
		return &budgetError{err: err}
	}
	return &spentError{scope: ScopeSpent}
}

// quotaRefusal reads a 429's message, which RapidAPI words as a quota exceeded when the plan's
// units are spent and as a rate limit otherwise, and closes the body when it is the former.
func quotaRefusal(response *http.Response) bool {
	body, err := io.ReadAll(io.LimitReader(response.Body, quotaBodyBytes))
	if err != nil {
		return false
	}
	if strings.Contains(strings.ToLower(string(body)), "quota") {
		return true
	}
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(strings.NewReader(string(body)), response.Body), response.Body}
	return false
}

// spentError is a lookup the budget refused before it was sent, naming the scope that refused.
type spentError struct {
	scope string
}

func (e *spentError) Error() string { return fmt.Sprintf("flight units spent for %s", e.scope) }

// budgetError is a failure to read or write the unit counters; the lookup is not sent.
type budgetError struct {
	err error
}

func (e *budgetError) Error() string { return "flight units: " + e.err.Error() }
func (e *budgetError) Unwrap() error { return e.err }
