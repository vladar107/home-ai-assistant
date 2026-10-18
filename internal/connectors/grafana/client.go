package grafana

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

	"github.com/vladar107/home-ai-assistant/internal/httpretry"
)

const (
	requestTimeout  = 20 * time.Second
	maximumAPIBytes = 8 << 20
	// maximumQueryErrorRunes bounds the query error passed to the model so it can fix its query.
	maximumQueryErrorRunes = 300
)

// apiError is a failed Grafana request reduced to a class; its text is safe for the model and the
// log, since it never carries the token, the address, or the response body. A query error is the
// one exception: it carries the data source's parse or execution message, which is about the
// query the model wrote and is what lets it correct one.
type apiError struct {
	class string
	text  string
}

func (e *apiError) Error() string { return e.text }

var (
	errUnauthorized = &apiError{"unauthorized", "Grafana refused the assistant's service-account token; the operator needs to check it."}
	errForbidden    = &apiError{"forbidden", "The assistant's Grafana service account is not allowed to read that."}
	errNotFound     = &apiError{"not_found", "Grafana does not know that dashboard or data source."}
	errRateLimited  = &apiError{"rate_limited", "Grafana is limiting requests right now; try again shortly."}
	errUnavailable  = &apiError{"unavailable", "Grafana is unavailable right now."}
	errTimeout      = &apiError{"timeout", "Grafana did not answer in time; try a shorter range or a narrower query."}
	errMalformed    = &apiError{"unexpected_response", "Grafana returned an unexpected response."}
	errTooLarge     = &apiError{"too_large", "The answer was too large to read; narrow the query or shorten the range."}
)

func queryError(message string) error {
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return &apiError{"bad_query", "The data source rejected the query."}
	}
	if runes := []rune(message); len(runes) > maximumQueryErrorRunes {
		message = string(runes[:maximumQueryErrorRunes]) + "…"
	}
	return &apiError{"bad_query", "The data source rejected the query: " + message}
}

type client struct {
	http  *http.Client
	base  *url.URL
	token string
}

func newClient(base *url.URL, token string) *client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &client{
		base:  base,
		token: token,
		http: &http.Client{
			Transport: httpretry.New(transport, httpretry.Options{Service: "grafana"}),
			Timeout:   requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// get sends one GET under the fixed base and decodes a 200 answer into into. A 400 or 422 from a
// data source is a query error and keeps the data source's own message. The status is classified
// before the size, so an oversized error page still reads as the failure it reports.
func (c *client) get(ctx context.Context, path string, query url.Values, into any) error {
	target := *c.base
	target.Path = path
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return errMalformed
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return transportError(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maximumAPIBytes+1))
	if err != nil {
		return transportError(err)
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if len(payload) > maximumAPIBytes {
			return queryError("")
		}
		return queryError(dataSourceMessage(payload))
	default:
		return statusError(response.StatusCode)
	}
	if len(payload) > maximumAPIBytes {
		return errTooLarge
	}
	if err := json.Unmarshal(payload, into); err != nil {
		return errMalformed
	}
	return nil
}

// dataSourceMessage is the error text of a rejected query: Prometheus answers with a JSON envelope,
// Loki with plain text.
func dataSourceMessage(payload []byte) string {
	var envelope struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(payload, &envelope) == nil {
		if envelope.Error != "" {
			return envelope.Error
		}
		return envelope.Message
	}
	return string(payload)
}

func statusError(status int) error {
	switch {
	case status == http.StatusUnauthorized:
		return errUnauthorized
	case status == http.StatusForbidden:
		return errForbidden
	case status == http.StatusNotFound:
		return errNotFound
	case status == http.StatusTooManyRequests:
		return errRateLimited
	case status >= 500:
		return errUnavailable
	default:
		return errMalformed
	}
}

func transportError(err error) error {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return errTimeout
	}
	return errUnavailable
}
