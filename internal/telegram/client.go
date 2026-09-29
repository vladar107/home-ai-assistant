package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	DefaultBaseURL    = "https://api.telegram.org"
	responseBodyLimit = 4 << 20
	httpTimeoutGrace  = 10 * time.Second
	documentNameLimit = 128
	// uploadTimeout is the ceiling for one upload. It is deliberately not the shared client's
	// timeout, which is sized for long polling: an upload is bounded by its caller's deadline,
	// and this only stops one that has no deadline at all from running forever.
	uploadTimeout = 2 * time.Minute
)

// filePathPattern accepts the relative path getFile returns and nothing that could escape the
// bot's own file route.
var filePathPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)*$`)

type ClientConfig struct {
	BaseURL     string
	Token       string
	PollTimeout time.Duration
	HTTPClient  *http.Client
	Telemetry   Telemetry
}

type Client struct {
	baseURL      *url.URL
	token        string
	pollSeconds  int64
	httpClient   *http.Client
	uploadClient *http.Client
	telemetry    Telemetry
}

type Bot struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// CallbackQuery is a button press. Message is the bot message that carried the button; Telegram
// may send only its chat and ID when the message is no longer accessible.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// InlineKeyboard is the rows of buttons shown under a message.
type InlineKeyboard [][]InlineButton

type Message struct {
	MessageID int64       `json:"message_id"`
	From      *User       `json:"from"`
	Chat      Chat        `json:"chat"`
	Text      string      `json:"text"`
	Caption   string      `json:"caption"`
	Photo     []PhotoSize `json:"photo"`
	Document  *MediaFile  `json:"document"`
	// The kinds below cannot reach the model through the runtime; they are read only so the
	// message can be answered instead of ignored.
	Voice     *MediaFile `json:"voice"`
	Audio     *MediaFile `json:"audio"`
	Video     *MediaFile `json:"video"`
	VideoNote *MediaFile `json:"video_note"`
	Sticker   *MediaFile `json:"sticker"`
	Animation *MediaFile `json:"animation"`
}

// PhotoSize is one rendition of a photo; Telegram lists them from smallest to largest.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

// MediaFile is any non-photo attachment. Telegram omits the name and type for some kinds.
type MediaFile struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// FileReference is what getFile returns: where the bot may download the file for the next hour.
type FileReference struct {
	FileID   string `json:"file_id"`
	FilePath string `json:"file_path"`
	FileSize int64  `json:"file_size"`
}

type Chat struct {
	ID   int64    `json:"id"`
	Type ChatType `json:"type"`
}

type ChatType string

const ChatTypePrivate ChatType = "private"

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type apiResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

// apiError keeps the description out of Error() so provider text never reaches a log.
type apiError struct {
	method      string
	httpStatus  int
	errorCode   int
	description string
}

func (e *apiError) Error() string {
	if e.errorCode != 0 {
		return fmt.Sprintf("Telegram %s returned HTTP status %d (API error code %d)", e.method, e.httpStatus, e.errorCode)
	}
	return fmt.Sprintf("Telegram %s returned HTTP status %d", e.method, e.httpStatus)
}

// NotModified reports Telegram's rejection of an edit that would change nothing.
func (e *apiError) NotModified() bool {
	return e.errorCode == http.StatusBadRequest && strings.Contains(strings.ToLower(e.description), "message is not modified")
}

// UnparsableEntities reports Telegram's rejection of the formatting entities of a message, which
// makes the same text deliverable only without a parse mode.
func (e *apiError) UnparsableEntities() bool {
	return e.errorCode == http.StatusBadRequest && strings.Contains(strings.ToLower(e.description), "can't parse entities")
}

func (e *apiError) Permanent() bool {
	if e.httpStatus == http.StatusTooManyRequests || e.httpStatus >= 500 ||
		e.errorCode == http.StatusTooManyRequests || e.errorCode >= 500 {
		return false
	}
	return e.errorCode >= 400 && e.errorCode < 500 ||
		e.httpStatus >= 400 && e.httpStatus < 500
}

func hasUnparsableEntities(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.UnparsableEntities()
}

func isPermanentAPIError(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Permanent()
}

func NewClient(cfg ClientConfig) (*Client, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	parsedBaseURL, err := url.Parse(baseURL)
	if err != nil || (parsedBaseURL.Scheme != "http" && parsedBaseURL.Scheme != "https") || parsedBaseURL.Host == "" {
		return nil, fmt.Errorf("Telegram base API URL must be an absolute HTTP or HTTPS URL")
	}
	if parsedBaseURL.User != nil || parsedBaseURL.RawQuery != "" || parsedBaseURL.Fragment != "" {
		return nil, fmt.Errorf("Telegram base API URL must not contain user information, a query, or a fragment")
	}
	if err := validateToken(cfg.Token); err != nil {
		return nil, err
	}
	if cfg.PollTimeout < time.Second || cfg.PollTimeout%time.Second != 0 {
		return nil, fmt.Errorf("Telegram poll timeout must be a positive whole number of seconds")
	}
	if cfg.PollTimeout > time.Duration(1<<63-1)-httpTimeoutGrace {
		return nil, fmt.Errorf("Telegram poll timeout is too large")
	}

	requiredHTTPTimeout := cfg.PollTimeout + httpTimeoutGrace
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout:       requiredHTTPTimeout,
			CheckRedirect: rejectRedirect,
		}
	} else {
		clientCopy := *httpClient
		if clientCopy.Timeout == 0 {
			clientCopy.Timeout = requiredHTTPTimeout
		}
		if clientCopy.Timeout <= cfg.PollTimeout {
			return nil, fmt.Errorf("Telegram HTTP client timeout must exceed the poll timeout")
		}
		clientCopy.CheckRedirect = rejectRedirect
		httpClient = &clientCopy
	}

	uploadClient := *httpClient
	uploadClient.Timeout = uploadTimeout

	return &Client{
		baseURL:      parsedBaseURL,
		token:        cfg.Token,
		pollSeconds:  int64(cfg.PollTimeout / time.Second),
		httpClient:   httpClient,
		uploadClient: &uploadClient,
		telemetry:    telemetryOrNoop(cfg.Telemetry),
	}, nil
}

func (c *Client) GetMe(ctx context.Context) (Bot, error) {
	return execute[Bot](c, ctx, "getMe", nil)
}

func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	params := url.Values{
		"allowed_updates": {`["message","callback_query"]`},
		"offset":          {strconv.FormatInt(offset, 10)},
		"timeout":         {strconv.FormatInt(c.pollSeconds, 10)},
	}
	return execute[[]Update](c, ctx, "getUpdates", params)
}

// GetFile resolves an attachment to a downloadable path. Telegram refuses files above 20 MB.
func (c *Client) GetFile(ctx context.Context, fileID string) (FileReference, error) {
	if strings.TrimSpace(fileID) == "" {
		return FileReference{}, fmt.Errorf("Telegram getFile requires a file identifier")
	}
	reference, err := execute[FileReference](c, ctx, "getFile", url.Values{"file_id": {fileID}})
	if err != nil {
		return FileReference{}, err
	}
	if !filePathPattern.MatchString(reference.FilePath) {
		return FileReference{}, fmt.Errorf("Telegram getFile returned an unusable file path")
	}
	return reference, nil
}

// DownloadFile reads an attachment from the bot's file route, refusing anything over the limit.
// The response is raw bytes rather than the JSON envelope every API method returns.
func (c *Client) DownloadFile(ctx context.Context, filePath string, limit int64) (result []byte, resultErr error) {
	if !filePathPattern.MatchString(filePath) {
		return nil, fmt.Errorf("Telegram file path is unusable")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("Telegram file size limit must be positive")
	}
	ctx, endTelemetry := c.telemetry.StartTelegramAPI(ctx, "downloadFile")
	defer func() {
		endTelemetry(resultErr)
	}()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.fileURL(filePath), nil)
	if err != nil {
		return nil, fmt.Errorf("Telegram file request could not be created")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("Telegram file request canceled: %w", ctxErr)
		}
		return nil, fmt.Errorf("Telegram file request failed")
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, &apiError{method: "downloadFile", httpStatus: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("Telegram file could not be read")
	}
	if int64(len(body)) > limit {
		return nil, &oversizedFileError{limit: limit}
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("Telegram file is empty")
	}
	return body, nil
}

// oversizedFileError reports a file the application refuses to hold, which is permanent for that
// message however often it is retried.
type oversizedFileError struct{ limit int64 }

func (e *oversizedFileError) Error() string {
	return fmt.Sprintf("Telegram file exceeds the %d byte limit", e.limit)
}

func (e *oversizedFileError) Permanent() bool { return true }

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) error {
	return c.SendReply(ctx, chatID, 0, text)
}

// SendReply quotes the request message with the reply. Telegram still delivers the text
// when that message was deleted, and a zero message ID sends without a quote.
func (c *Client) SendReply(ctx context.Context, chatID, replyToMessageID int64, text string) error {
	return c.send(ctx, chatID, replyToMessageID, text, nil)
}

// SendKeyboard sends a message with inline buttons, quoting the request like SendReply.
func (c *Client) SendKeyboard(ctx context.Context, chatID, replyToMessageID int64, text string, keyboard InlineKeyboard) error {
	return c.send(ctx, chatID, replyToMessageID, text, keyboard)
}

func (c *Client) send(ctx context.Context, chatID, replyToMessageID int64, text string, keyboard InlineKeyboard) error {
	_, err := c.sendMessage(ctx, chatID, replyToMessageID, text, keyboard)
	return err
}

func (c *Client) sendMessage(ctx context.Context, chatID, replyToMessageID int64, text string, keyboard InlineKeyboard) (Message, error) {
	params := url.Values{"chat_id": {strconv.FormatInt(chatID, 10)}}
	if replyToMessageID > 0 {
		params.Set("reply_parameters", fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, replyToMessageID))
	}
	if err := setKeyboard(params, keyboard); err != nil {
		return Message{}, err
	}
	setFormattedText(params, text)
	message, err := execute[Message](c, ctx, "sendMessage", params)
	if hasUnparsableEntities(err) {
		setPlainText(params, text)
		return execute[Message](c, ctx, "sendMessage", params)
	}
	return message, err
}

// SendProposal posts a confirmation request with Confirm and Reject buttons and returns the
// message ID so the outcome can replace the buttons later.
func (c *Client) SendProposal(ctx context.Context, chatID int64, text, confirmData, rejectData string) (int64, error) {
	message, err := c.sendMessage(ctx, chatID, 0, text, InlineKeyboard{{
		{Text: "Confirm", CallbackData: confirmData},
		{Text: "Reject", CallbackData: rejectData},
	}})
	if err != nil {
		return 0, err
	}
	if message.MessageID <= 0 {
		return 0, fmt.Errorf("Telegram sendMessage returned no message ID")
	}
	return message.MessageID, nil
}

// EditText replaces a bot message's text and removes its buttons.
func (c *Client) EditText(ctx context.Context, chatID, messageID int64, text string) error {
	return c.EditKeyboard(ctx, chatID, messageID, text, nil)
}

// EditKeyboard replaces the text and buttons of a bot message in place. An edit that changes
// nothing is a success; an edit Telegram no longer allows is a permanent API error.
func (c *Client) EditKeyboard(ctx context.Context, chatID, messageID int64, text string, keyboard InlineKeyboard) error {
	params := url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"message_id": {strconv.FormatInt(messageID, 10)},
	}
	if err := setKeyboard(params, keyboard); err != nil {
		return err
	}
	err := c.editMessageText(ctx, params, text)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.NotModified() {
		return nil
	}
	return err
}

func (c *Client) editMessageText(ctx context.Context, params url.Values, text string) error {
	setFormattedText(params, text)
	_, err := execute[json.RawMessage](c, ctx, "editMessageText", params)
	if hasUnparsableEntities(err) {
		setPlainText(params, text)
		_, err = execute[json.RawMessage](c, ctx, "editMessageText", params)
	}
	return err
}

// DeleteMessage removes a message from the chat. Telegram lets a bot delete any message in a
// private chat for 48 hours after it was sent.
func (c *Client) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	params := url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"message_id": {strconv.FormatInt(messageID, 10)},
	}
	_, err := execute[bool](c, ctx, "deleteMessage", params)
	return err
}

// AnswerCallback closes the loading state of a button press, with an optional short toast.
func (c *Client) AnswerCallback(ctx context.Context, callbackID, text string) error {
	params := url.Values{"callback_query_id": {callbackID}}
	if text != "" {
		params.Set("text", text)
	}
	_, err := execute[bool](c, ctx, "answerCallbackQuery", params)
	return err
}

// setFormattedText renders the message as Telegram HTML, and setPlainText restores the text the
// caller wrote. A message Telegram refuses to parse is resent plain, so a formatting fault can
// never swallow an answer the user is waiting for.
func setFormattedText(params url.Values, text string) {
	params.Set("text", renderMarkup(text))
	params.Set("parse_mode", "HTML")
}

func setPlainText(params url.Values, text string) {
	params.Set("text", text)
	params.Del("parse_mode")
}

func setKeyboard(params url.Values, keyboard InlineKeyboard) error {
	if len(keyboard) == 0 {
		return nil
	}
	encoded, err := json.Marshal(struct {
		InlineKeyboard InlineKeyboard `json:"inline_keyboard"`
	}{keyboard})
	if err != nil {
		return fmt.Errorf("Telegram inline keyboard could not be encoded")
	}
	params.Set("reply_markup", string(encoded))
	return nil
}

func (c *Client) SendTyping(ctx context.Context, chatID int64) error {
	params := url.Values{
		"action":  {"typing"},
		"chat_id": {strconv.FormatInt(chatID, 10)},
	}
	_, err := execute[bool](c, ctx, "sendChatAction", params)
	return err
}

// SendDocument uploads a file to the chat as a Telegram document. The caller bounds the size; the
// body is streamed into the multipart request between its prologue and closing boundary so a file
// never has to be held in memory, which is also why the length is set rather than left to chunked
// encoding.
func (c *Client) SendDocument(ctx context.Context, chatID int64, name string, body io.Reader, size int64) (resultErr error) {
	if chatID == 0 {
		return fmt.Errorf("Telegram sendDocument requires a chat")
	}
	if body == nil {
		return fmt.Errorf("Telegram sendDocument requires a file to upload")
	}
	if size <= 0 {
		return fmt.Errorf("Telegram sendDocument requires a positive file size")
	}
	ctx, endTelemetry := c.telemetry.StartTelegramAPI(ctx, "sendDocument")
	defer func() {
		endTelemetry(resultErr)
	}()

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return fmt.Errorf("Telegram sendDocument request could not be created")
	}
	if _, err := writer.CreateFormFile("document", documentName(name)); err != nil {
		return fmt.Errorf("Telegram sendDocument request could not be created")
	}
	prologueEnd := form.Len()
	if err := writer.Close(); err != nil {
		return fmt.Errorf("Telegram sendDocument request could not be created")
	}
	encoded := form.Bytes()

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.endpointURL("sendDocument"),
		io.MultiReader(bytes.NewReader(encoded[:prologueEnd]), io.LimitReader(body, size), bytes.NewReader(encoded[prologueEnd:])),
	)
	if err != nil {
		return fmt.Errorf("Telegram sendDocument request could not be created")
	}
	request.ContentLength = int64(len(encoded)) + size
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", writer.FormDataContentType())

	response, err := c.uploadClient.Do(request)
	if err != nil {
		return requestFailure(ctx, "sendDocument", err)
	}
	defer response.Body.Close()

	_, err = decodeResponse[Message]("sendDocument", response)
	return err
}

// SendPhoto uploads a PNG to the chat as a photo, quoting the request like SendReply. The caption
// is rendered like a message and, when Telegram refuses its entities, resent as the text those
// entities would have shown, which is what CaptionFits measured.
func (c *Client) SendPhoto(ctx context.Context, chatID, replyToMessageID int64, png []byte, caption string) error {
	if chatID == 0 {
		return fmt.Errorf("Telegram sendPhoto requires a chat")
	}
	if len(png) == 0 {
		return fmt.Errorf("Telegram sendPhoto requires a photo to upload")
	}
	fields := map[string]string{"chat_id": strconv.FormatInt(chatID, 10)}
	if replyToMessageID > 0 {
		fields["reply_parameters"] = fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, replyToMessageID)
	}
	rendered := renderMarkup(caption)
	if strings.TrimSpace(caption) != "" {
		fields["caption"] = rendered
		fields["parse_mode"] = "HTML"
	}
	err := c.sendPhoto(ctx, fields, png)
	if hasUnparsableEntities(err) {
		fields["caption"] = captionText(rendered)
		delete(fields, "parse_mode")
		err = c.sendPhoto(ctx, fields, png)
	}
	return err
}

func (c *Client) sendPhoto(ctx context.Context, fields map[string]string, png []byte) (resultErr error) {
	ctx, endTelemetry := c.telemetry.StartTelegramAPI(ctx, "sendPhoto")
	defer func() {
		endTelemetry(resultErr)
	}()

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			return fmt.Errorf("Telegram sendPhoto request could not be created")
		}
	}
	part, err := writer.CreateFormFile("photo", "card.png")
	if err != nil {
		return fmt.Errorf("Telegram sendPhoto request could not be created")
	}
	if _, err := part.Write(png); err != nil {
		return fmt.Errorf("Telegram sendPhoto request could not be created")
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("Telegram sendPhoto request could not be created")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL("sendPhoto"), &form)
	if err != nil {
		return fmt.Errorf("Telegram sendPhoto request could not be created")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", writer.FormDataContentType())

	response, err := c.uploadClient.Do(request)
	if err != nil {
		return requestFailure(ctx, "sendPhoto", err)
	}
	defer response.Body.Close()

	_, err = decodeResponse[Message]("sendPhoto", response)
	return err
}

// CaptionFits reports whether text can be a photo caption. Telegram measures a caption after
// parsing its entities, in UTF-16 code units, so the limit applies to what the user will see rather
// than to the markup.
func (c *Client) CaptionFits(text string) bool {
	return utf16Length(captionText(renderMarkup(text))) <= captionLimit
}

const captionLimit = 1024

// captionText is the text Telegram shows for rendered markup: the tags dropped and the entities
// renderMarkup escaped turned back into characters. renderMarkup escapes every angle bracket it
// does not write as a tag, so each one left is a tag boundary.
func captionText(rendered string) string {
	var text strings.Builder
	inTag := false
	for _, r := range rendered {
		switch {
		case r == '<':
			inTag = true
		case r == '>' && inTag:
			inTag = false
		case !inTag:
			text.WriteRune(r)
		}
	}
	return html.UnescapeString(text.String())
}

func utf16Length(text string) int {
	length := 0
	for _, r := range text {
		length += utf16.RuneLen(r)
	}
	return length
}

// documentName makes a name the user chose usable in a part header. multipart escapes quotes and
// backslashes, but a control character would still break the header apart.
func documentName(name string) string {
	sanitized := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, name)
	if runes := []rune(sanitized); len(runes) > documentNameLimit {
		sanitized = string(runes[:documentNameLimit])
	}
	sanitized = strings.TrimSpace(sanitized)
	if sanitized == "" {
		return "file"
	}
	return sanitized
}

// CommandScope names a Bot API BotCommandScope type; the empty scope is the default scope.
type CommandScope string

const CommandScopeAllPrivateChats CommandScope = "all_private_chats"

func (c *Client) SetMyCommands(ctx context.Context, scope CommandScope, commands []BotCommand) error {
	encoded, err := json.Marshal(commands)
	if err != nil {
		return fmt.Errorf("Telegram setMyCommands command list could not be encoded")
	}
	params := url.Values{"commands": {string(encoded)}}
	if scope != "" {
		encodedScope, err := json.Marshal(struct {
			Type string `json:"type"`
		}{string(scope)})
		if err != nil {
			return fmt.Errorf("Telegram setMyCommands scope could not be encoded")
		}
		params.Set("scope", string(encodedScope))
	}
	_, err = execute[bool](c, ctx, "setMyCommands", params)
	return err
}

func execute[T any](c *Client, ctx context.Context, method string, params url.Values) (result T, resultErr error) {
	var zero T
	ctx, endTelemetry := c.telemetry.StartTelegramAPI(ctx, method)
	defer func() {
		endTelemetry(resultErr)
	}()

	if params == nil {
		params = make(url.Values)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.endpointURL(method),
		strings.NewReader(params.Encode()),
	)
	if err != nil {
		return zero, fmt.Errorf("Telegram %s request could not be created", method)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return zero, requestFailure(ctx, method, err)
	}
	defer response.Body.Close()

	return decodeResponse[T](method, response)
}

func requestFailure(ctx context.Context, method string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("Telegram %s request canceled: %w", method, ctxErr)
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return fmt.Errorf("Telegram %s request timed out", method)
	}
	cause := err
	if requestError, ok := err.(*url.Error); ok && requestError.Err != nil {
		cause = requestError.Err
	}
	return fmt.Errorf("Telegram %s request failed: %w", method, cause)
}

func decodeResponse[T any](method string, response *http.Response) (T, error) {
	var zero T
	body, err := io.ReadAll(io.LimitReader(response.Body, responseBodyLimit+1))
	if err != nil {
		return zero, fmt.Errorf("Telegram %s response could not be read (HTTP status %d)", method, response.StatusCode)
	}
	if len(body) > responseBodyLimit {
		return zero, fmt.Errorf("Telegram %s response exceeded the size limit (HTTP status %d)", method, response.StatusCode)
	}

	var envelope apiResponse[T]
	if err := json.Unmarshal(body, &envelope); err != nil {
		return zero, fmt.Errorf("Telegram %s returned invalid JSON (HTTP status %d)", method, response.StatusCode)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || !envelope.OK {
		return zero, &apiError{method: method, httpStatus: response.StatusCode, errorCode: envelope.ErrorCode, description: envelope.Description}
	}

	return envelope.Result, nil
}

// fileURL builds the download route, which carries the token in the path like every API method
// but serves file bytes rather than a JSON envelope.
func (c *Client) fileURL(filePath string) string {
	endpoint := *c.baseURL
	basePath := strings.TrimRight(endpoint.Path, "/")
	escapedBasePath := strings.TrimRight(endpoint.EscapedPath(), "/")
	endpoint.Path = basePath + "/file/bot" + c.token + "/" + filePath
	endpoint.RawPath = escapedBasePath + "/file/bot" + url.PathEscape(c.token) + "/" + escapeFilePath(filePath)
	return endpoint.String()
}

func escapeFilePath(filePath string) string {
	segments := strings.Split(filePath, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

func (c *Client) endpointURL(method string) string {
	endpoint := *c.baseURL
	basePath := strings.TrimRight(endpoint.Path, "/")
	escapedBasePath := strings.TrimRight(endpoint.EscapedPath(), "/")
	endpoint.Path = basePath + "/bot" + c.token + "/" + method
	endpoint.RawPath = escapedBasePath + "/bot" + url.PathEscape(c.token) + "/" + method
	return endpoint.String()
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func validateToken(token string) error {
	if token == "" || !utf8.ValidString(token) {
		return fmt.Errorf("Telegram token is invalid")
	}
	if strings.IndexFunc(token, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return fmt.Errorf("Telegram token is invalid")
	}
	return nil
}
