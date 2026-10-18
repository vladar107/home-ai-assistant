package gmail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
)

const (
	DefaultAPIBaseURL = "https://gmail.googleapis.com"

	product = "Gmail"

	requestTimeout    = 10 * time.Second
	responseBodyLimit = 4 << 20
)

func apiError(class google.ErrorClass, status int) error {
	return &google.Error{Product: product, Class: class, Status: status}
}

// errorClass is the class of a failed Gmail API request, and empty for any other error.
func errorClass(err error) google.ErrorClass {
	if gmailErr, ok := google.ProductError(err, product); ok {
		return gmailErr.Class
	}
	return ""
}

type ClientConfig struct {
	APIBaseURL string
	HTTPClient *http.Client
}

// Client talks to the Gmail API with a principal's access token passed on every call; it holds no
// user credential itself.
type Client struct {
	apiBaseURL string
	httpClient *http.Client
}

func NewClient(cfg ClientConfig) (*Client, error) {
	base, err := google.Endpoint(cfg.APIBaseURL, DefaultAPIBaseURL)
	if err != nil {
		return nil, err
	}
	client := &Client{apiBaseURL: base, httpClient: cfg.HTTPClient}
	if client.httpClient == nil {
		client.httpClient = &http.Client{Timeout: requestTimeout + time.Second, Transport: httpretry.New(nil, httpretry.Options{Service: "gmail"})}
	}
	return client, nil
}

func (c *Client) send(request *http.Request, out any) (int, error) {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, apiError(google.ErrorUnavailable, 0)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, responseBodyLimit+1))
	if err != nil || len(contents) > responseBodyLimit {
		return response.StatusCode, apiError(google.ErrorUnavailable, response.StatusCode)
	}
	if len(contents) > 0 && json.Unmarshal(contents, out) != nil && response.StatusCode == http.StatusOK {
		return response.StatusCode, apiError(google.ErrorUnavailable, response.StatusCode)
	}
	return response.StatusCode, nil
}

func (c *Client) get(ctx context.Context, access, path string, query url.Values, out any) error {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	target := c.apiBaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target, nil)
	if err != nil {
		return apiError(google.ErrorUnavailable, 0)
	}
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Accept", "application/json")
	var raw json.RawMessage
	status, err := c.send(request, &raw)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return apiError(google.Classify(status), status)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return apiError(google.ErrorUnavailable, status)
	}
	return nil
}

type messageList struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	NextPageToken      string `json:"nextPageToken"`
	ResultSizeEstimate int    `json:"resultSizeEstimate"`
}

func (c *Client) ListMessages(ctx context.Context, access, query string, limit int) (messageList, error) {
	var list messageList
	err := c.get(ctx, access, "/gmail/v1/users/me/messages", url.Values{
		"q":          {query},
		"maxResults": {strconv.Itoa(limit)},
	}, &list)
	return list, err
}

// Message is the part of a Gmail message resource the tools read.
type Message struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds"`
	Snippet      string   `json:"snippet"`
	InternalDate string   `json:"internalDate"`
	Payload      Part     `json:"payload"`
	// Account is the address of the mailbox the message was read from.
	Account string `json:"-"`
}

type Part struct {
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
	Headers  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Body struct {
		Size         int64  `json:"size"`
		Data         string `json:"data"`
		AttachmentID string `json:"attachmentId"`
	} `json:"body"`
	Parts []Part `json:"parts"`
}

var metadataHeaders = []string{"From", "To", "Cc", "Subject", "Date"}

// MessageMetadata reads headers, labels, and the snippet without the body.
func (c *Client) MessageMetadata(ctx context.Context, access, id string) (Message, error) {
	var message Message
	err := c.get(ctx, access, "/gmail/v1/users/me/messages/"+url.PathEscape(id), url.Values{
		"format":          {"metadata"},
		"metadataHeaders": metadataHeaders,
	}, &message)
	return message, err
}

func (c *Client) MessageFull(ctx context.Context, access, id string) (Message, error) {
	var message Message
	err := c.get(ctx, access, "/gmail/v1/users/me/messages/"+url.PathEscape(id), url.Values{"format": {"full"}}, &message)
	return message, err
}
