package flights

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/flightnumber"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
)

const (
	defaultBaseURL = "https://aerodatabox.p.rapidapi.com"
	// rapidAPIHost names the API to RapidAPI whatever base the requests go to.
	rapidAPIHost = "aerodatabox.p.rapidapi.com"

	requestTimeout  = 15 * time.Second
	maximumAPIBytes = 1 << 20
)

// Provider looks flights up by number. The day is a local date, YYYY-MM-DD, and the role says
// whether it is the day of departure, or of either departure or arrival, each local to its
// airport. A flight the provider does not know is ErrNotFound.
type Provider interface {
	Flights(ctx context.Context, designator, day string, role Role) ([]Leg, error)
}

// apiError is a failed lookup reduced to a class; its text is safe for the model and the log,
// since it never carries the key, the address, or the response body.
type apiError struct {
	class string
	text  string
}

func (e *apiError) Error() string { return e.text }

var (
	// ErrUnauthorized is a key the provider refused; every later lookup is skipped until restart.
	ErrUnauthorized = &apiError{"unauthorized", "The flight status provider refused the operator's key; the operator needs to check it."}
	// ErrNotFound is a flight the provider has no record of on that day.
	// ErrNotSubscribed is a key RapidAPI refuses for an API it is not subscribed to; like a refused
	// key, it stops every lookup until restart.
	ErrNotSubscribed = &apiError{"not_subscribed", "The operator's RapidAPI key is not subscribed to AeroDataBox; the operator needs to subscribe it."}
	ErrNotFound      = &apiError{"not_found", "The flight status provider has no flight under that number on that day."}
	errRateLimited   = &apiError{"rate_limited", "The flight status request limit has been reached; try again in a minute."}
	errUnavailable   = &apiError{"unavailable", "Flight status is unavailable right now."}
	errTimeout       = &apiError{"timeout", "The flight status provider did not respond in time."}
	errMalformed     = &apiError{"unexpected_response", "The flight status provider returned an unexpected response."}
)

// aeroDataBox is the AeroDataBox flight status API on RapidAPI.
type aeroDataBox struct {
	http *http.Client
	base *url.URL
	key  string
}

var _ Provider = (*aeroDataBox)(nil)

func newAeroDataBox(base *url.URL, key string, metered *meteredTransport) *aeroDataBox {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	metered.next = transport
	return &aeroDataBox{
		base: base,
		key:  key,
		http: &http.Client{
			Transport: httpretry.New(metered, httpretry.Options{Service: "flights"}),
			Timeout:   requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Flights asks for the flight status of one number on one local day, without the aircraft's
// image, its position, or its flight plan, which would bill the request twice.
func (a *aeroDataBox) Flights(ctx context.Context, designator, day string, role Role) ([]Leg, error) {
	target := *a.base
	target.Path = strings.TrimRight(a.base.Path, "/") + "/flights/number/" + url.PathEscape(designator) + "/" + url.PathEscape(day)
	target.RawQuery = url.Values{
		"withAircraftImage": {"false"},
		"withLocation":      {"false"},
		"withFlightPlan":    {"false"},
		"dateLocalRole":     {string(role)},
	}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errMalformed
	}
	request.Header.Set("X-RapidAPI-Key", a.key)
	request.Header.Set("X-RapidAPI-Host", rapidAPIHost)
	request.Header.Set("Accept", "application/json")
	response, err := a.http.Do(request)
	if err != nil {
		return nil, transportError(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumAPIBytes+1))
	if err != nil {
		return nil, transportError(err)
	}
	switch {
	case response.StatusCode == http.StatusNoContent:
		return nil, ErrNotFound
	case response.StatusCode != http.StatusOK:
		return nil, statusError(response.StatusCode)
	case len(body) > maximumAPIBytes:
		return nil, errMalformed
	}
	var flights []adbFlight
	if err := json.Unmarshal(body, &flights); err != nil {
		return nil, errMalformed
	}
	legs := make([]Leg, 0, len(flights))
	for _, flight := range flights {
		if leg, ok := flight.leg(designator); ok {
			legs = append(legs, leg)
		}
	}
	switch {
	case len(flights) == 0:
		return nil, ErrNotFound
	case len(legs) == 0:
		return nil, errMalformed
	}
	return legs, nil
}

func statusError(status int) error {
	switch {
	case status == http.StatusUnauthorized:
		return ErrUnauthorized
	case status == http.StatusForbidden:
		return ErrNotSubscribed
	case status == http.StatusTooManyRequests:
		return errRateLimited
	case status == http.StatusUnavailableForLegalReasons || status >= 500:
		return errUnavailable
	default:
		return errMalformed
	}
}

// transportError keeps what the metered transport decided and drops any other underlying error,
// whose text names the address.
func transportError(err error) error {
	var spent *spentError
	var budget *budgetError
	var netErr net.Error
	switch {
	case errors.As(err, &spent):
		return spent
	case errors.As(err, &budget):
		return budget
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return errTimeout
	}
	return errUnavailable
}

// adbFlight is one flight of AeroDataBox's answer, with only the fields the connector reads.
type adbFlight struct {
	Number          string `json:"number"`
	Status          string `json:"status"`
	CodeshareStatus string `json:"codeshareStatus"`
	Airline         struct {
		Name string `json:"name"`
	} `json:"airline"`
	Departure adbMovement `json:"departure"`
	Arrival   adbMovement `json:"arrival"`
}

type adbMovement struct {
	Airport struct {
		IATA         string `json:"iata"`
		ICAO         string `json:"icao"`
		Name         string `json:"name"`
		Municipality string `json:"municipalityName"`
		TimeZone     string `json:"timeZone"`
	} `json:"airport"`
	ScheduledTime *adbTime `json:"scheduledTime"`
	RevisedTime   *adbTime `json:"revisedTime"`
	PredictedTime *adbTime `json:"predictedTime"`
	RunwayTime    *adbTime `json:"runwayTime"`
	Terminal      string   `json:"terminal"`
	Gate          string   `json:"gate"`
	Quality       []string `json:"quality"`
}

type adbTime struct {
	UTC string `json:"utc"`
}

// instant reads a UTC time as AeroDataBox writes it, such as "2026-11-09 12:40Z"; false means it
// is present but unreadable.
func (t *adbTime) instant() (time.Time, bool) {
	if t == nil || t.UTC == "" {
		return time.Time{}, true
	}
	for _, layout := range []string{"2006-01-02 15:04Z07:00", "2006-01-02 15:04:05Z07:00", time.RFC3339} {
		if parsed, err := time.Parse(layout, t.UTC); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func (f adbFlight) leg(asked string) (Leg, bool) {
	departure, ok := f.Departure.movement()
	if !ok {
		return Leg{}, false
	}
	arrival, ok := f.Arrival.movement()
	if !ok || departure.Scheduled.IsZero() {
		return Leg{}, false
	}
	designator, valid := flightnumber.Normalize(f.Number)
	if !valid {
		designator = asked
	}
	leg := Leg{
		Designator: designator,
		Airline:    plainText(f.Airline.Name, 80),
		Departure:  departure,
		Arrival:    arrival,
		Live:       slices.Contains(f.Departure.Quality, "Live") || slices.Contains(f.Arrival.Quality, "Live"),
		Status:     StatusUnknown,
		Codeshare:  CodeshareUnknown,
	}
	switch f.CodeshareStatus {
	case "IsOperator":
		leg.Codeshare = CodeshareOperator
	case "IsCodeshared":
		leg.Codeshare = CodeshareMarketed
	}
	switch f.Status {
	case StatusExpected, StatusEnRoute, StatusCheckIn, StatusBoarding, StatusGateClosed, StatusDeparted, StatusDelayed,
		StatusApproaching, StatusArrived, StatusCanceled, StatusDiverted, StatusCanceledUncertain:
		leg.Status = f.Status
	}
	return leg, true
}

func (m adbMovement) movement() (Movement, bool) {
	var movement Movement
	for _, read := range []struct {
		from *adbTime
		into *time.Time
	}{
		{m.ScheduledTime, &movement.Scheduled}, {m.RevisedTime, &movement.Revised},
		{m.PredictedTime, &movement.Predicted}, {m.RunwayTime, &movement.Runway},
	} {
		at, ok := read.from.instant()
		if !ok {
			return Movement{}, false
		}
		*read.into = at
	}
	movement.Airport = Airport{
		Code: plainText(cmp.Or(strings.TrimSpace(m.Airport.IATA), strings.TrimSpace(m.Airport.ICAO)), 4),
		Name: plainText(cmp.Or(strings.TrimSpace(m.Airport.Municipality), strings.TrimSpace(m.Airport.Name)), 80),
		Zone: plainText(m.Airport.TimeZone, 64),
	}
	movement.Gate, movement.Terminal = plainText(m.Gate, 12), plainText(m.Terminal, 12)
	return movement, true
}

// plainText keeps a provider's text to one short line of printable characters.
func plainText(value string, limit int) string {
	value = strings.Join(strings.FieldsFunc(value, func(r rune) bool { return r < ' ' || r == 0x7f || r == ' ' }), " ")
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return strings.TrimSpace(string(runes))
}
