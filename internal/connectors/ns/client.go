package ns

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	requestTimeout = 15 * time.Second
	// The national disruption list without isActive, which a future date_time needs, was
	// 783 KB on 2026-09-28, almost all of it planned maintenance.
	maximumAPIBytes = 4 << 20

	invalidDateError = "INVALID_TRAVELADVICE_DATE"
)

// apiError is a failed NS request reduced to a class; its text is safe for the model and the log,
// since it never carries the key, the address, or the response body.
type apiError struct {
	class string
	text  string
}

func (e *apiError) Error() string { return e.text }

var (
	errUnauthorized = &apiError{"unauthorized", "NS refused the operator's API key; the operator needs to check it."}
	errNotFound     = &apiError{"not_found", "NS does not know that station or request."}
	errRateLimited  = &apiError{"rate_limited", "The NS request limit has been reached; try again later."}
	errUnavailable  = &apiError{"unavailable", "NS travel information is unavailable right now."}
	errBadRequest   = &apiError{"bad_request", "NS could not answer that request."}
	errNoPlan       = &apiError{"invalid_date", "NS has no travel plan for that date."}
	errTimeout      = &apiError{"timeout", "NS did not respond in time."}
	errMalformed    = &apiError{"unexpected_response", "NS returned an unexpected response."}
)

type client struct {
	http *http.Client
	base *url.URL
	key  string
}

func newClient(base *url.URL, key string) *client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &client{
		base: base,
		key:  key,
		http: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// get calls one endpoint under the fixed base and decodes its JSON answer into into.
func (c *client) get(ctx context.Context, path string, query url.Values, into any) error {
	target := *c.base
	target.Path = strings.TrimRight(c.base.Path, "/") + "/" + path
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return errMalformed
	}
	request.Header.Set("Ocp-Apim-Subscription-Key", c.key)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return transportError(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumAPIBytes+1))
	if err != nil {
		return transportError(err)
	}
	if response.StatusCode != http.StatusOK {
		return statusError(response.StatusCode, body)
	}
	if len(body) > maximumAPIBytes {
		return errMalformed
	}
	if err := json.Unmarshal(body, into); err != nil {
		return errMalformed
	}
	return nil
}

func statusError(status int, body []byte) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return errUnauthorized
	case status == http.StatusNotFound:
		return errNotFound
	case status == http.StatusTooManyRequests:
		return errRateLimited
	case status == http.StatusBadRequest:
		var payload struct {
			Errors []struct {
				Type string `json:"type"`
			} `json:"errors"`
		}
		if json.Unmarshal(body, &payload) == nil {
			for _, item := range payload.Errors {
				if item.Type == invalidDateError {
					return errNoPlan
				}
			}
		}
		return errBadRequest
	case status >= 500:
		return errUnavailable
	default:
		return errMalformed
	}
}

// transportError drops the underlying error, whose text names the address.
func transportError(err error) error {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return errTimeout
	}
	return errUnavailable
}

func errorClass(err error) string {
	var failure *apiError
	if errors.As(err, &failure) {
		return failure.class
	}
	return "other"
}

// modelText is the sentence the model sees for a failed request.
func modelText(err error) string {
	var failure *apiError
	if errors.As(err, &failure) {
		return failure.text
	}
	return errUnavailable.text
}

type stationRecord struct {
	UICCode string `json:"UICCode"`
	Code    string `json:"code"`
	Land    string `json:"land"`
	Namen   struct {
		Lang   string `json:"lang"`
		Middel string `json:"middel"`
		Kort   string `json:"kort"`
	} `json:"namen"`
	Synoniemen []string `json:"synoniemen"`
}

func (c *client) stations(ctx context.Context) ([]stationRecord, error) {
	var response struct {
		Payload []stationRecord `json:"payload"`
	}
	if err := c.get(ctx, "v2/stations", nil, &response); err != nil {
		return nil, err
	}
	if len(response.Payload) == 0 {
		return nil, errMalformed
	}
	return response.Payload, nil
}

func formatQueryTime(at time.Time) string {
	return at.Format("2006-01-02T15:04:05-07:00")
}

func parseTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}
