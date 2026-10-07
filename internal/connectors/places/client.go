package places

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/httpretry"
)

const maximumAPIBytes = 4 << 20

// apiError is a failed request reduced to a class; its text is safe for the model and the log,
// since it never carries the address or the response body.
type apiError struct {
	class string
	text  string
}

func (e *apiError) Error() string { return e.text }

var (
	errRateLimited = &apiError{"rate_limited", "The places service is limiting requests."}
	errUnavailable = &apiError{"unavailable", "The places service is unavailable."}
	errBadRequest  = &apiError{"bad_request", "The places service could not answer that request."}
	errTimeout     = &apiError{"timeout", "The places service did not respond in time."}
	errMalformed   = &apiError{"unexpected_response", "The places service returned an unexpected response."}
)

func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Transport: httpretry.New(transport, httpretry.Options{Service: "places"}),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (s *Service) getJSON(ctx context.Context, base *url.URL, path string, query url.Values, into any) error {
	target := *base
	target.Path = strings.TrimRight(base.Path, "/") + "/" + path
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return errMalformed
	}
	return s.doJSON(request, into)
}

// postForm sends the form to the base address itself, as Overpass expects its query.
func (s *Service) postForm(ctx context.Context, base *url.URL, form url.Values, into any) error {
	request, err := http.NewRequestWithContext(httpretry.Idempotent(ctx), http.MethodPost, base.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return errMalformed
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return s.doJSON(request, into)
}

func (s *Service) doJSON(request *http.Request, into any) error {
	request.Header.Set("User-Agent", s.agent)
	request.Header.Set("Accept", "application/json")
	response, err := s.http.Do(request)
	if err != nil {
		return transportError(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumAPIBytes+1))
	if err != nil {
		return transportError(err)
	}
	if response.StatusCode != http.StatusOK {
		return statusError(response.StatusCode)
	}
	if len(body) > maximumAPIBytes {
		return errMalformed
	}
	if err := json.Unmarshal(body, into); err != nil {
		return errMalformed
	}
	return nil
}

func statusError(status int) error {
	switch {
	case status == http.StatusTooManyRequests:
		return errRateLimited
	case status == http.StatusBadRequest:
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
