package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	requestTimeout = 15 * time.Second
	// A /tpc answer for a large interchange such as Amsterdam Centraal, and alerts.pb at about
	// 390 KB on 2026-10-02, both fit with room to spare.
	maximumAPIBytes = 4 << 20
)

// apiError is a failed request reduced to a class; its text is safe for the model and the log,
// since it never carries the address or the response body.
type apiError struct {
	class string
	text  string
}

func (e *apiError) Error() string { return e.text }

var (
	errNotFound    = &apiError{"not_found", "The bus service does not know that stop or request."}
	errRateLimited = &apiError{"rate_limited", "The bus service is limiting requests; try again later."}
	errUnavailable = &apiError{"unavailable", "Bus information is unavailable right now."}
	errBadRequest  = &apiError{"bad_request", "The bus service could not answer that request."}
	errTimeout     = &apiError{"timeout", "The bus service did not respond in time."}
	errMalformed   = &apiError{"unexpected_response", "The bus service returned an unexpected response."}
)

type client struct {
	http  *http.Client
	base  *url.URL
	agent string
}

func newClient(base *url.URL, agent string) *client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &client{
		base:  base,
		agent: agent,
		http: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c *client) request(ctx context.Context, method, path string, query url.Values) (*http.Request, error) {
	target := *c.base
	target.Path = strings.TrimRight(c.base.Path, "/") + "/" + path
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return nil, errMalformed
	}
	request.Header.Set("User-Agent", c.agent)
	return request, nil
}

// fetch calls one endpoint under the fixed base and returns its body, at most limit bytes.
func (c *client) fetch(ctx context.Context, path string, query url.Values, limit int64) ([]byte, error) {
	request, err := c.request(ctx, http.MethodGet, path, query)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, transportError(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, transportError(err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, statusError(response.StatusCode)
	}
	if int64(len(body)) > limit {
		return nil, errMalformed
	}
	return body, nil
}

func (c *client) getJSON(ctx context.Context, path string, query url.Values, into any) error {
	request, err := c.request(ctx, http.MethodGet, path, query)
	if err != nil {
		return err
	}
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

// rangeReader reads a remote file through HTTP range requests in aligned blocks, so a zip reader
// can pull a few entries out of an archive far too large to download. It stops after budget bytes,
// which bounds the cost of an archive laid out in an unexpected way.
type rangeReader struct {
	ctx    context.Context
	client *client
	path   string
	size   int64
	budget int64

	blockStart int64
	block      []byte
}

const rangeBlockBytes = 1 << 20

func (c *client) openRange(ctx context.Context, path string, budget int64) (*rangeReader, error) {
	reader := &rangeReader{ctx: ctx, client: c, path: path, budget: budget, blockStart: -1}
	// The first block is the archive's tail, where a zip keeps its directory; its Content-Range
	// also gives the size.
	request, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Range", "bytes=-"+strconv.Itoa(rangeBlockBytes))
	body, contentRange, err := reader.do(request)
	if err != nil {
		return nil, err
	}
	first, size, ok := parseContentRange(contentRange)
	if !ok || first+int64(len(body)) != size {
		return nil, errMalformed
	}
	reader.size, reader.blockStart, reader.block = size, first, body
	return reader, nil
}

func (r *rangeReader) Size() int64 { return r.size }

func (r *rangeReader) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errMalformed
	}
	read := 0
	for read < len(p) {
		at := offset + int64(read)
		if at >= r.size {
			return read, io.EOF
		}
		if r.blockStart < 0 || at < r.blockStart || at >= r.blockStart+int64(len(r.block)) {
			if err := r.load(at - at%rangeBlockBytes); err != nil {
				return read, err
			}
		}
		read += copy(p[read:], r.block[at-r.blockStart:])
	}
	return read, nil
}

func (r *rangeReader) load(start int64) error {
	end := min(start+rangeBlockBytes, r.size) - 1
	request, err := r.client.request(r.ctx, http.MethodGet, r.path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	body, contentRange, err := r.do(request)
	if err != nil {
		return err
	}
	first, size, ok := parseContentRange(contentRange)
	if !ok || first != start || size != r.size || int64(len(body)) != end-start+1 {
		return errMalformed
	}
	r.blockStart, r.block = start, body
	return nil
}

// do sends one range request, refusing a server that answers with the whole file.
func (r *rangeReader) do(request *http.Request) ([]byte, string, error) {
	response, err := r.client.http.Do(request)
	if err != nil {
		return nil, "", transportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		if response.StatusCode == http.StatusOK {
			return nil, "", errMalformed
		}
		return nil, "", statusError(response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, rangeBlockBytes+1))
	if err != nil {
		return nil, "", transportError(err)
	}
	if len(body) > rangeBlockBytes {
		return nil, "", errMalformed
	}
	r.budget -= int64(len(body))
	if r.budget < 0 {
		return nil, "", errMalformed
	}
	return body, response.Header.Get("Content-Range"), nil
}

// parseContentRange reads "bytes <first>-<last>/<size>".
func parseContentRange(value string) (first, size int64, ok bool) {
	span, total, found := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	if !found {
		return 0, 0, false
	}
	start, _, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, false
	}
	first, err := strconv.ParseInt(start, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	size, err = strconv.ParseInt(total, 10, 64)
	if err != nil || first < 0 || first >= size {
		return 0, 0, false
	}
	return first, size, true
}

func statusError(status int) error {
	switch {
	case status == http.StatusNotFound:
		return errNotFound
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
	var failure *apiError
	if errors.As(err, &failure) {
		return failure
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
