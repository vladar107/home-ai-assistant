package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/httpretry"
	"github.com/vladar107/home-ai-assistant/internal/integration"
)

const (
	DefaultAPIBaseURL = "https://api.notion.com"
	apiVersion        = "2026-03-11"
	requestTimeout    = 10 * time.Second
	maximumRetryAfter = 5 * time.Second
	MaximumPageSize   = 25
	responseBodyLimit = 1 << 20
)

type ClientConfig struct {
	BaseURL    string
	HTTPClient *http.Client
}

// Client is the typed Notion API client. Every call carries the principal's token explicitly; the
// client itself holds no credential.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(cfg ClientConfig) (*Client, error) {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = DefaultAPIBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("Notion API base URL must be a plain HTTP or HTTPS URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout + time.Second, Transport: httpretry.New(nil, httpretry.Options{Service: "notion"})}
	}
	return &Client{baseURL: parsed.String(), httpClient: httpClient}, nil
}

// Bot is the non-secret identity of a token as reported by GET /v1/users/me.
type Bot struct {
	ID            string
	Name          string
	OwnerType     string
	WorkspaceID   string
	WorkspaceName string
}

// TokenKind maps the owner reported by Notion to the connection kinds the settings show: an
// internal connection is workspace-owned, a personal access token is owned by its creator.
func (b Bot) TokenKind() string {
	if b.OwnerType == "user" {
		return integration.TokenKindPersonal
	}
	return integration.TokenKindInternal
}

// RichText is a rich text run. Notion returns the flattened run in PlainText, while a write input
// carries the text the model wrote in Text.Content.
type RichText struct {
	PlainText string `json:"plain_text"`
	Href      string `json:"href"`
	Text      struct {
		Content string `json:"content"`
	} `json:"text"`
}

// Object is a page or a data source as Notion returns it. A page's properties are values; a
// data source's properties are its schema.
type Object struct {
	Object         string                     `json:"object"`
	ID             string                     `json:"id"`
	URL            string                     `json:"url"`
	CreatedTime    string                     `json:"created_time"`
	LastEditedTime string                     `json:"last_edited_time"`
	InTrash        bool                       `json:"in_trash"`
	Title          []RichText                 `json:"title"`
	Properties     map[string]json.RawMessage `json:"properties"`
	Parent         Parent                     `json:"parent"`
}

type Parent struct {
	Type         string `json:"type"`
	DataSourceID string `json:"data_source_id"`
	DatabaseID   string `json:"database_id"`
	PageID       string `json:"page_id"`
	BlockID      string `json:"block_id"`
}

type Block struct {
	ID          string
	Type        string
	HasChildren bool
	Content     json.RawMessage
}

// Database is a database container as Notion returns it. Since API version 2025-09-03 a database
// holds the data sources that actually carry rows, so its ID cannot be queried directly.
type Database struct {
	ID          string          `json:"id"`
	Title       []RichText      `json:"title"`
	IsInline    bool            `json:"is_inline"`
	DataSources []DataSourceRef `json:"data_sources"`
	Parent      Parent          `json:"parent"`
}

type DataSourceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (b *Block) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var header struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		HasChildren bool   `json:"has_children"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	b.ID, b.Type, b.HasChildren = header.ID, header.Type, header.HasChildren
	b.Content = raw[header.Type]
	return nil
}

type List[T any] struct {
	Results    []T    `json:"results"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

type SearchRequest struct {
	Query      string
	ObjectType string
	PageSize   int
	Cursor     string
}

type QueryRequest struct {
	Filter   json.RawMessage
	Sorts    json.RawMessage
	PageSize int
	Cursor   string
}

func (c *Client) Me(ctx context.Context, token string) (Bot, error) {
	var response struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Bot  struct {
			Owner struct {
				Type string `json:"type"`
			} `json:"owner"`
			WorkspaceID   string `json:"workspace_id"`
			WorkspaceName string `json:"workspace_name"`
		} `json:"bot"`
	}
	if err := c.do(ctx, token, http.MethodGet, "/v1/users/me", nil, &response); err != nil {
		return Bot{}, err
	}
	if response.ID == "" || response.Bot.WorkspaceID == "" {
		return Bot{}, &Error{Class: ErrorUnavailable}
	}
	return Bot{
		ID:            response.ID,
		Name:          response.Name,
		OwnerType:     response.Bot.Owner.Type,
		WorkspaceID:   response.Bot.WorkspaceID,
		WorkspaceName: response.Bot.WorkspaceName,
	}, nil
}

func (c *Client) Search(ctx context.Context, token string, request SearchRequest) (List[Object], error) {
	body := map[string]any{"page_size": pageSize(request.PageSize)}
	if request.Query != "" {
		body["query"] = request.Query
	}
	if request.ObjectType != "" {
		body["filter"] = map[string]string{"property": "object", "value": request.ObjectType}
	}
	if request.Cursor != "" {
		body["start_cursor"] = request.Cursor
	}
	var result List[Object]
	err := c.do(httpretry.Idempotent(ctx), token, http.MethodPost, "/v1/search", body, &result)
	return result, err
}

func (c *Client) Page(ctx context.Context, token, pageID string) (Object, error) {
	var page Object
	err := c.do(ctx, token, http.MethodGet, "/v1/pages/"+url.PathEscape(pageID), nil, &page)
	return page, err
}

// Database retrieves a database container so its data sources can be queried. A child_database
// block's ID is a database ID, which no query endpoint accepts.
func (c *Client) Database(ctx context.Context, token, databaseID string) (Database, error) {
	var database Database
	err := c.do(ctx, token, http.MethodGet, "/v1/databases/"+url.PathEscape(databaseID), nil, &database)
	return database, err
}

// DataSource retrieves one data source. Its properties are the schema of the rows it holds, so
// they carry the writable structure and the options every select, status, and multi-select
// property accepts.
func (c *Client) DataSource(ctx context.Context, token, dataSourceID string) (Object, error) {
	var dataSource Object
	err := c.do(ctx, token, http.MethodGet, "/v1/data_sources/"+url.PathEscape(dataSourceID), nil, &dataSource)
	return dataSource, err
}

func (c *Client) BlockChildren(ctx context.Context, token, blockID, cursor string) (List[Block], error) {
	query := url.Values{"page_size": {"100"}}
	if cursor != "" {
		query.Set("start_cursor", cursor)
	}
	var result List[Block]
	err := c.do(ctx, token, http.MethodGet, "/v1/blocks/"+url.PathEscape(blockID)+"/children?"+query.Encode(), nil, &result)
	return result, err
}

func (c *Client) QueryDataSource(ctx context.Context, token, dataSourceID string, request QueryRequest) (List[Object], error) {
	body := map[string]any{"page_size": pageSize(request.PageSize)}
	if len(request.Filter) != 0 {
		body["filter"] = request.Filter
	}
	if len(request.Sorts) != 0 {
		body["sorts"] = request.Sorts
	}
	if request.Cursor != "" {
		body["start_cursor"] = request.Cursor
	}
	var result List[Object]
	err := c.do(httpretry.Idempotent(ctx), token, http.MethodPost, "/v1/data_sources/"+url.PathEscape(dataSourceID)+"/query", body, &result)
	return result, err
}

func pageSize(requested int) int {
	if requested <= 0 || requested > MaximumPageSize {
		return MaximumPageSize
	}
	return requested
}

// do performs one request with the ten-second deadline and a single retry on 429 or 529 when
// Notion's Retry-After fits within the retry allowance.
func (c *Client) do(ctx context.Context, token, method, path string, body any, out any) error {
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Notion request: %w", err)
		}
	}
	for attempt := 0; ; attempt++ {
		retryAfter, err := c.attempt(ctx, token, method, path, encoded, out)
		var notionErr *Error
		if attempt == 0 && errorAs(err, &notionErr) && notionErr.Class == ErrorRateLimited && retryAfter > 0 && retryAfter <= maximumRetryAfter {
			if !wait(ctx, retryAfter) {
				return &Error{Class: ErrorUnavailable}
			}
			continue
		}
		return err
	}
}

func (c *Client) attempt(ctx context.Context, token, method, path string, body []byte, out any) (time.Duration, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(requestCtx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, &Error{Class: ErrorUnavailable}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Notion-Version", apiVersion)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, &Error{Class: ErrorUnavailable}
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, responseBodyLimit+1))
	if err != nil || len(contents) > responseBodyLimit {
		return 0, &Error{Class: ErrorUnavailable, Status: response.StatusCode}
	}
	if response.StatusCode != http.StatusOK {
		return parseRetryAfter(response.Header.Get("Retry-After")), &Error{Class: classify(response.StatusCode), Status: response.StatusCode}
	}
	if err := json.Unmarshal(contents, out); err != nil {
		return 0, &Error{Class: ErrorUnavailable, Status: response.StatusCode}
	}
	return 0, nil
}

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// CreatePage creates one page from a Notion create-page body. A failure after the request was
// sent is ambiguous, so callers never retry it.
func (c *Client) CreatePage(ctx context.Context, token string, body map[string]any) (Object, error) {
	var page Object
	err := c.do(ctx, token, http.MethodPost, "/v1/pages", body, &page)
	return page, err
}

func (c *Client) UpdatePage(ctx context.Context, token, pageID string, properties map[string]json.RawMessage) (Object, error) {
	var page Object
	err := c.do(ctx, token, http.MethodPatch, "/v1/pages/"+url.PathEscape(pageID), map[string]any{"properties": properties}, &page)
	return page, err
}

func (c *Client) AppendBlockChildren(ctx context.Context, token, blockID string, children []map[string]any) (List[Block], error) {
	var result List[Block]
	err := c.do(ctx, token, http.MethodPatch, "/v1/blocks/"+url.PathEscape(blockID)+"/children", map[string]any{"children": children}, &result)
	return result, err
}
