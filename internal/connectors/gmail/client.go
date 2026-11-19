package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	return c.do(request, access, out)
}

func (c *Client) do(request *http.Request, access string, out any) error {
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

// HeaderValues returns every value of the top-level header, matched without regard to case, since
// Gmail keeps the sender's spelling, such as Message-Id.
func (m Message) HeaderValues(name string) []string {
	var values []string
	for _, candidate := range m.Payload.Headers {
		if strings.EqualFold(candidate.Name, name) {
			values = append(values, candidate.Value)
		}
	}
	return values
}

var metadataHeaders = []string{"From", "To", "Cc", "Subject", "Date"}

// InboundHeaders are the headers the assistant's own mailbox reads to route a message to its
// principals and announce it.
var InboundHeaders = []string{
	"Delivered-To", "To", "Cc", "From", "Reply-To", "Subject", "Date",
	"Authentication-Results", "Message-ID", "References", "In-Reply-To",
}

// MessageMetadata reads headers, labels, and the snippet without the body.
func (c *Client) MessageMetadata(ctx context.Context, access, id string) (Message, error) {
	return c.MessageHeaders(ctx, access, id, metadataHeaders)
}

// MessageHeaders reads the named headers, labels, and the snippet without the body.
func (c *Client) MessageHeaders(ctx context.Context, access, id string, headers []string) (Message, error) {
	var message Message
	err := c.get(ctx, access, "/gmail/v1/users/me/messages/"+url.PathEscape(id), url.Values{
		"format":          {"metadata"},
		"metadataHeaders": headers,
	}, &message)
	return message, err
}

func (c *Client) MessageFull(ctx context.Context, access, id string) (Message, error) {
	var message Message
	err := c.get(ctx, access, "/gmail/v1/users/me/messages/"+url.PathEscape(id), url.Values{"format": {"full"}}, &message)
	return message, err
}

// MessageRef identifies a message with the labels it carries.
type MessageRef struct {
	ID       string   `json:"id"`
	ThreadID string   `json:"threadId"`
	LabelIDs []string `json:"labelIds"`
}

// Send sends raw, a complete RFC 5322 message, in the thread when threadID is not empty; Gmail
// files it there only when its In-Reply-To or References points into the thread and its subject
// matches, and may give it its own Message-ID, so read that back with MessageHeaders. It is sent
// once and never repeated. A failure of class ErrorUnavailable (a refused or broken connection, a
// timeout, a 5xx, or a 200 whose body cannot be read) means the message was possibly sent; any
// other class, such as ErrorInvalid, ErrorUnauthorized, ErrorForbidden, ErrorNotFound or
// ErrorRateLimited, means Gmail refused it and nothing was sent.
func (c *Client) Send(ctx context.Context, access string, raw []byte, threadID string) (MessageRef, error) {
	body, err := json.Marshal(struct {
		Raw      string `json:"raw"`
		ThreadID string `json:"threadId,omitempty"`
	}{base64.URLEncoding.EncodeToString(raw), threadID})
	if err != nil {
		return MessageRef{}, apiError(google.ErrorInvalid, 0)
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.apiBaseURL+"/gmail/v1/users/me/messages/send", bytes.NewReader(body))
	if err != nil {
		return MessageRef{}, apiError(google.ErrorUnavailable, 0)
	}
	// Gmail's send takes no idempotency key, so a repeat can deliver the message twice. A request
	// without GetBody is never sent again by the retrying transport or by net/http, even after a
	// failure that proves nothing reached Gmail.
	request.GetBody = nil
	request.Header.Set("Content-Type", "application/json")
	var sent MessageRef
	err = c.do(request, access, &sent)
	return sent, err
}

// ErrHistoryExpired is History's failure when Gmail no longer keeps the changes after the start
// id, which it answers with 404. The error also carries the Gmail not-found class.
var ErrHistoryExpired = errors.New("gmail history start id expired")

// HistoryPage is one page of the messages added to the mailbox after a history id.
type HistoryPage struct {
	Added         []MessageRef
	NextPageToken string
	// HistoryID is the mailbox's current history id.
	HistoryID string
}

// History lists the messages added after startHistoryID, a page at a time. A start id Gmail no
// longer keeps fails with ErrHistoryExpired; the caller then lists the mailbox again and restarts
// from Profile's history id.
func (c *Client) History(ctx context.Context, access, startHistoryID, pageToken string) (HistoryPage, error) {
	query := url.Values{"startHistoryId": {startHistoryID}, "historyTypes": {"messageAdded"}}
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}
	var body struct {
		History []struct {
			MessagesAdded []struct {
				Message MessageRef `json:"message"`
			} `json:"messagesAdded"`
		} `json:"history"`
		NextPageToken string `json:"nextPageToken"`
		HistoryID     string `json:"historyId"`
	}
	if err := c.get(ctx, access, "/gmail/v1/users/me/history", query, &body); err != nil {
		if errorClass(err) == google.ErrorNotFound {
			return HistoryPage{}, fmt.Errorf("%w: %w", ErrHistoryExpired, err)
		}
		return HistoryPage{}, err
	}
	page := HistoryPage{NextPageToken: body.NextPageToken, HistoryID: body.HistoryID}
	for _, record := range body.History {
		for _, added := range record.MessagesAdded {
			page.Added = append(page.Added, added.Message)
		}
	}
	return page, nil
}

// Profile is the mailbox's address and its current history id.
type Profile struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    string `json:"historyId"`
}

// Profile reads the mailbox's address and the history id to start reading History from.
func (c *Client) Profile(ctx context.Context, access string) (Profile, error) {
	var profile Profile
	err := c.get(ctx, access, "/gmail/v1/users/me/profile", nil, &profile)
	return profile, err
}
