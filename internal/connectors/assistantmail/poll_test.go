package assistantmail_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connectors/assistantmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
)

const (
	mailbox  = "assistant@example.com"
	redirect = "http://127.0.0.1:49152/callback"
	verifier = "test-verifier-0123456789abcdefghijklmnopqrstuvwxyz"
)

type harness struct {
	t        *testing.T
	ctx      context.Context
	sim      *googlesim.Simulator
	store    *sqlite.Store
	switches *integration.Switches
	gmail    *gmail.Client
	service  *assistantmail.Service
	metrics  *metrics
	logs     *syncBuffer
	chat     *chatSender
	notices  *assistantmail.Notices
	address  string
	cfg      assistantmail.Config
}

func newHarness(t *testing.T, configure ...func(*assistantmail.Config)) *harness {
	t.Helper()
	return newHarnessAt(t, mailbox, configure...)
}

// newHarnessAt is a harness whose mailbox has the address.
func newHarnessAt(t *testing.T, address string, configure ...func(*assistantmail.Config)) *harness {
	t.Helper()
	ctx := t.Context()
	sim := googlesim.New(t)
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	switches, err := integration.NewSwitches(store, logger)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := google.NewClient(google.ClientConfig{
		ClientID: googlesim.AssistantClientID, ClientSecret: googlesim.AssistantClientSecret, RedirectURL: redirect,
		TokenURL: sim.TokenURL(), HTTPClient: sim.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := gmail.NewClient(gmail.ClientConfig{APIBaseURL: sim.URL(), HTTPClient: sim.Client()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := assistantmail.Config{
		Mailbox: config.AssistantMailConfig{
			Enabled: true, Address: address, DisplayName: "Assistant", ClientID: googlesim.AssistantClientID,
			ClientSecret: googlesim.AssistantClientSecret, RefreshToken: authorize(t, sim, tokens, address),
			Tags:           map[string][]string{"alice": {"a1", "a0"}, "bob": {"b1"}},
			DailySendLimit: 10, PollInterval: 5 * time.Minute,
		},
		Repository: store, Gmail: client, Tokens: tokens, Switches: switches, Chats: &chatDirectory{reader: store}, Sender: &chatSender{},
		Languages: store, Location: noticeZone, Notices: assistantmail.NewNotices(), Logger: logger,
	}
	for _, change := range configure {
		change(&cfg)
	}
	h := &harness{t: t, ctx: ctx, sim: sim, store: store, switches: switches, gmail: client, logs: logs, address: address, cfg: cfg}
	h.restart()
	return h
}

// restart builds the service again over the same store, simulator, chat, and metrics, as a new
// process would.
func (h *harness) restart() {
	h.t.Helper()
	service, err := assistantmail.NewService(h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	if h.metrics == nil {
		h.metrics = &metrics{polled: make(chan string, 16)}
	}
	service.SetTelemetry(h.metrics)
	h.service = service
	h.chat, _ = h.cfg.Sender.(*chatSender)
	h.notices = h.cfg.Notices
}

// authorize plays the operator's one-time authorization of the mailbox's own client and returns
// its refresh token.
func authorize(t *testing.T, sim *googlesim.Simulator, tokens *google.Client, account string) string {
	t.Helper()
	challenge := sha256.Sum256([]byte(verifier))
	link := sim.AuthURL() + "?" + url.Values{
		"client_id":             {googlesim.AssistantClientID},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"scope":                 {google.MailScope + " " + google.SendScope},
		"access_type":           {"offline"},
		"state":                 {"state"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}.Encode()
	back, err := url.Parse(sim.Consent(link, account, googlesim.Grant{Mail: true, Send: true}))
	if err != nil {
		t.Fatal(err)
	}
	granted, err := tokens.Exchange(t.Context(), back.Query().Get("code"), verifier)
	if err != nil || granted.Refresh == "" {
		t.Fatalf("exchange = %+v %v", granted, err)
	}
	return granted.Refresh
}

// poll polls once and waits for the triage it started.
func (h *harness) poll() {
	h.t.Helper()
	if err := h.service.Poll(h.ctx); err != nil {
		h.t.Fatalf("poll: %v", err)
	}
	if err := h.service.WaitTriage(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

// start runs the first poll, which only sets the cursor.
func (h *harness) start() {
	h.t.Helper()
	h.poll()
	if _, found, err := h.store.HistoryCursor(h.ctx); err != nil || !found {
		h.t.Fatalf("cursor after the first poll = %v %v", found, err)
	}
}

func (h *harness) deliver(recipient string, message googlesim.Message) string {
	h.t.Helper()
	if message.From == "" {
		message.From = "Shop <orders@shop.example.org>"
	}
	if message.Subject == "" {
		message.Subject = "Your order"
	}
	return h.sim.DeliverTo(recipient, message)
}

// routes returns the principals the message was routed to.
func (h *harness) routes(gmailID string) []string {
	h.t.Helper()
	var principals []string
	for _, principal := range []string{"alice", "bob"} {
		route, found, err := h.store.Route(h.ctx, principal, gmailID)
		if err != nil {
			h.t.Fatal(err)
		}
		if found {
			if route.Direction != assistantmail.DirectionIn {
				h.t.Fatalf("%s route direction = %q", principal, route.Direction)
			}
			principals = append(principals, principal)
		}
	}
	return principals
}

func (h *harness) expectRoutes(gmailID string, want ...string) {
	h.t.Helper()
	if got := h.routes(gmailID); !slices.Equal(got, want) {
		h.t.Fatalf("routes of %s = %v, want %v", gmailID, got, want)
	}
}

func (h *harness) cursor() string {
	h.t.Helper()
	cursor, found, err := h.store.HistoryCursor(h.ctx)
	if err != nil || !found {
		h.t.Fatalf("cursor = %v %v", found, err)
	}
	return cursor
}

// expireCursor makes Gmail forget the history from the stored cursor on.
func (h *harness) expireCursor() {
	h.t.Helper()
	cursor, err := strconv.ParseUint(h.cursor(), 10, 64)
	if err != nil {
		h.t.Fatal(err)
	}
	h.sim.ExpireHistoryBefore(h.address, strconv.FormatUint(cursor+1, 10))
}

func (h *harness) listRequests() int {
	count := 0
	for _, request := range h.sim.Requests() {
		if request.Path == "/gmail/v1/users/me/messages" {
			count++
		}
	}
	return count
}

// googleRequests counts the requests made to Google, token refreshes included.
func (h *harness) googleRequests() int {
	return len(h.sim.Requests())
}

// headerReads counts the reads that route the message, which ask for its Delivered-To; a notice
// reads other headers.
func (h *harness) headerReads(gmailID string) int {
	count := 0
	for _, request := range h.sim.Requests() {
		if request.Path == "/gmail/v1/users/me/messages/"+gmailID && slices.Contains(request.Query["metadataHeaders"], "Delivered-To") {
			count++
		}
	}
	return count
}

func (h *harness) expectQuietLogs() {
	h.t.Helper()
	logs := h.logs.String()
	for _, secret := range []string{"example.com", "example.org", "a1", "b1", "Your order", "Subject"} {
		if strings.Contains(logs, secret) {
			h.t.Fatalf("logs mention %q:\n%s", secret, logs)
		}
	}
}

func TestFirstPollStartsFromTheProfileAndIgnoresOlderMail(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.deliver("assistant+a1@example.com", googlesim.Message{})

	h.start()
	h.poll()
	h.expectRoutes(old)
	if _, found, err := h.store.NewestSeenInternalDate(h.ctx); err != nil || found {
		t.Fatalf("older mail was recorded: %v %v", found, err)
	}

	fresh := h.deliver("assistant+a1@example.com", googlesim.Message{})
	h.poll()
	h.expectRoutes(fresh, "alice")
	h.expectRoutes(old)
	h.expectQuietLogs()
}

func TestRoutingFollowsTheTagsOnTheMailboxAddress(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()

	delivered := h.deliver("assistant+a1@example.com", googlesim.Message{To: "Customers <list@shop.example.org>"})
	inTo := h.deliver(mailbox, googlesim.Message{To: "Assistant <ASSISTANT+B1@Example.com>"})
	inCc := h.deliver(mailbox, googlesim.Message{To: "friend@example.net", Cc: "Assistant <assistant+a1@example.com>"})
	both := h.deliver("assistant+a1@example.com", googlesim.Message{To: "assistant+a1@example.com, assistant+b1@example.com"})
	retired := h.deliver("assistant+a0@example.com", googlesim.Message{})
	elsewhere := h.deliver(mailbox, googlesim.Message{To: "other+a1@example.com, assistant+a1@example.net"})
	unknown := h.deliver("assistant+zz9@example.com", googlesim.Message{})
	h.poll()

	h.expectRoutes(delivered, "alice")
	h.expectRoutes(inTo, "bob")
	h.expectRoutes(inCc, "alice")
	h.expectRoutes(both, "alice", "bob")
	h.expectRoutes(retired, "alice")
	h.expectRoutes(elsewhere)
	h.expectRoutes(unknown)
	if got := h.metrics.count("routed"); got != 5 {
		t.Fatalf("routed = %d", got)
	}
	if got := h.metrics.count("unrouted"); got != 2 {
		t.Fatalf("unrouted = %d", got)
	}
	h.expectQuietLogs()
}

func TestAReplyToThePlainAddressFollowsTheSentThread(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	access := h.sim.Access(mailbox, googlesim.Grant{Mail: true, Send: true})
	raw := "From: Assistant <assistant@example.com>\r\nReply-To: assistant+b1@example.com\r\nTo: desk@shop.example.org\r\n" +
		"Subject: Opening hours\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nWhen are you open?\r\n"
	sent, err := h.gmail.Send(h.ctx, access, []byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.RecordSent(h.ctx, sent.ID, sent.ThreadID, "bob", time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	headers, err := h.gmail.MessageHeaders(h.ctx, access, sent.ID, []string{"Message-ID"})
	if err != nil || len(headers.HeaderValues("Message-ID")) != 1 {
		t.Fatalf("sent message headers = %+v %v", headers, err)
	}
	messageID := headers.HeaderValues("Message-ID")[0]

	reply := h.deliver(mailbox, googlesim.Message{Subject: "Re: Opening hours", InReplyTo: messageID, References: messageID})
	stranger := h.deliver(mailbox, googlesim.Message{Subject: "Hello"})
	h.poll()

	h.expectRoutes(reply, "bob")
	h.expectRoutes(stranger)
	if got := h.metrics.count("unrouted"); got != 1 {
		t.Fatalf("unrouted = %d", got)
	}
	// The sent message was recorded at send, so the poller adds nothing for it.
	if got := h.metrics.count("skipped"); got != 0 {
		t.Fatalf("skipped = %d", got)
	}
	routes, err := h.store.ListMailRoutes(h.ctx, "bob", 10)
	if err != nil || len(routes) != 2 {
		t.Fatalf("bob's routes = %+v %v", routes, err)
	}
}

func TestSpamIsRoutedAndMarkedAndTrashDraftAndSentAreSkipped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	spam := h.deliver("assistant+a1@example.com", googlesim.Message{Labels: []string{"SPAM", "UNREAD"}})
	trash := h.deliver("assistant+a1@example.com", googlesim.Message{Labels: []string{"TRASH"}})
	draft := h.deliver("assistant+a1@example.com", googlesim.Message{Labels: []string{"DRAFT"}})
	selfSent := h.deliver("assistant+a1@example.com", googlesim.Message{Labels: []string{"SENT"}})
	toSelf := h.deliver("assistant+b1@example.com", googlesim.Message{Labels: []string{"SENT", "INBOX"}})
	h.poll()

	route, found, err := h.store.Route(h.ctx, "alice", spam)
	if err != nil || !found || !route.Spam {
		t.Fatalf("spam route = %+v %v %v", route, found, err)
	}
	for _, id := range []string{trash, draft, selfSent} {
		h.expectRoutes(id)
	}
	h.expectRoutes(toSelf, "bob")
	if spam, skipped, routed := h.metrics.count("spam"), h.metrics.count("skipped"), h.metrics.count("routed"); spam != 1 || skipped != 3 || routed != 1 {
		t.Fatalf("spam %d, skipped %d, routed %d", spam, skipped, routed)
	}
	if newest, found, err := h.store.NewestSeenInternalDate(h.ctx); err != nil || !found || newest.IsZero() {
		t.Fatalf("skipped messages are not recorded as seen: %v %v %v", newest, found, err)
	}
}

func TestARepeatedHistoryIDIsProcessedOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	id := h.deliver("assistant+a1@example.com", googlesim.Message{})
	h.sim.RepeatInHistory(mailbox, id)
	h.sim.RepeatInHistory(mailbox, id)
	h.poll()

	h.expectRoutes(id, "alice")
	if reads := h.headerReads(id); reads != 1 {
		t.Fatalf("header reads = %d", reads)
	}
	if got := h.metrics.count("routed"); got != 1 {
		t.Fatalf("routed = %d", got)
	}
}

func TestHistoryIsReadAcrossPagesBeforeTheCursorMoves(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	h.sim.SetHistoryPageSize(2)
	var ids []string
	for range 5 {
		ids = append(ids, h.deliver("assistant+b1@example.com", googlesim.Message{}))
	}
	before := h.cursor()
	h.poll()

	for _, id := range ids {
		h.expectRoutes(id, "bob")
	}
	pages := 0
	for _, request := range h.sim.Requests() {
		if request.Path == "/gmail/v1/users/me/history" {
			pages++
		}
	}
	if pages != 3 {
		t.Fatalf("history pages = %d", pages)
	}
	after := h.cursor()
	if before == after {
		t.Fatalf("cursor stayed at %s", after)
	}

	// Nothing new: the next poll reads one empty page and records nothing.
	h.poll()
	if got := h.metrics.count("routed"); got != 5 {
		t.Fatalf("routed = %d", got)
	}
}

func TestAnExpiredCursorRecoversWithoutDuplicates(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	first := h.deliver("assistant+a1@example.com", googlesim.Message{})
	h.poll()
	h.expectRoutes(first, "alice")

	second := h.deliver("assistant+a1@example.com", googlesim.Message{})
	spam := h.deliver("assistant+b1@example.com", googlesim.Message{Labels: []string{"SPAM"}})
	stale := h.cursor()
	h.expireCursor()
	h.poll()

	h.expectRoutes(first, "alice")
	h.expectRoutes(second, "alice")
	h.expectRoutes(spam, "bob")
	routes, err := h.store.ListMailRoutes(h.ctx, "alice", 10)
	if err != nil || len(routes) != 2 {
		t.Fatalf("alice's routes = %+v %v", routes, err)
	}
	if routed, spammed := h.metrics.count("routed"), h.metrics.count("spam"); routed != 2 || spammed != 1 {
		t.Fatalf("routed %d, spam %d", routed, spammed)
	}
	if got := h.metrics.polls("recovered"); got != 1 {
		t.Fatalf("recovered polls = %d", got)
	}
	if reads := h.headerReads(first); reads != 1 {
		t.Fatalf("an already recorded message was read %d times", reads)
	}
	recovered := h.cursor()
	if recovered == stale {
		t.Fatal("the cursor was not reset")
	}

	third := h.deliver("assistant+b1@example.com", googlesim.Message{})
	h.poll()
	h.expectRoutes(third, "bob")
	if got := h.metrics.polls("recovered"); got != 1 {
		t.Fatalf("recovered polls after the reset = %d", got)
	}
}

func TestAnExpiredCursorWithNothingSeenListsSinceTheCursorWasSaved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	stale := h.cursor()
	id := h.deliver("assistant+b1@example.com", googlesim.Message{})
	h.expireCursor()
	h.poll()

	h.expectRoutes(id, "bob")
	if h.cursor() == stale {
		t.Fatal("the cursor was not reset")
	}
}

func TestMailIndexedLateIsFoundByTheNextPollAfterARecovery(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	first := h.deliver("assistant+a1@example.com", googlesim.Message{})
	h.poll()

	late := h.deliver("assistant+a1@example.com", googlesim.Message{})
	h.sim.IndexLate(late, 1)
	h.expireCursor()
	h.poll()
	h.expectRoutes(late)

	h.poll()
	h.expectRoutes(late, "alice")
	h.expectRoutes(first, "alice")
	if reads := h.headerReads(first); reads != 1 {
		t.Fatalf("an already recorded message was read %d times", reads)
	}

	h.poll()
	if lists := h.listRequests(); lists != 2 {
		t.Fatalf("searches = %d, want the recovery and one repeat", lists)
	}
}

func TestARejectedRefreshTokenMakesTheMailboxUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	h.sim.RevokeGrant(mailbox)
	h.sim.ExpireAccessTokens()
	h.deliver("assistant+a1@example.com", googlesim.Message{})

	if err := h.service.Poll(h.ctx); !errors.Is(err, assistantmail.ErrUnavailable) {
		t.Fatalf("poll = %v", err)
	}
	if h.service.Available() || h.service.Unavailable() == "" {
		t.Fatal("the mailbox still reports itself available")
	}
	requests := h.googleRequests()
	if err := h.service.Poll(h.ctx); !errors.Is(err, assistantmail.ErrUnavailable) {
		t.Fatalf("second poll = %v", err)
	}
	if got := h.googleRequests(); got != requests {
		t.Fatalf("requests after the rejection = %d, want %d", got, requests)
	}
	if got := h.metrics.refreshFailures("rejected"); got != 1 {
		t.Fatalf("rejected refreshes = %d", got)
	}
	if strings.Count(h.logs.String(), "assistant_mail.refresh.rejected") != 1 {
		t.Fatalf("logs:\n%s", h.logs.String())
	}
}

func TestATokenForAnotherAccountMakesTheMailboxUnavailable(t *testing.T) {
	t.Parallel()
	// Gmail ignores dots in its own addresses and reads googlemail.com as gmail.com, so the token
	// reads the configured mailbox.
	same := newHarnessAt(t, "my.assistant@gmail.com", func(cfg *assistantmail.Config) { cfg.Mailbox.Address = "MyAssistant@googlemail.com" })
	same.start()
	same.poll()
	if !same.service.Available() {
		t.Fatal("the same mailbox under another spelling is unavailable")
	}

	h := newHarness(t)
	h.start()
	h.cfg.Mailbox.Address = "helper@example.com"
	h.restart()
	id := h.deliver("assistant+a1@example.com", googlesim.Message{})
	if err := h.service.Poll(h.ctx); !errors.Is(err, assistantmail.ErrUnavailable) {
		t.Fatalf("poll = %v", err)
	}
	if h.service.Available() || !strings.Contains(h.service.Unavailable(), "another Google account") {
		t.Fatalf("unavailable = %q", h.service.Unavailable())
	}
	if reads := h.headerReads(id); reads != 0 {
		t.Fatalf("another account's message was read %d times", reads)
	}
	requests := h.googleRequests()
	if err := h.service.Poll(h.ctx); !errors.Is(err, assistantmail.ErrUnavailable) {
		t.Fatalf("second poll = %v", err)
	}
	if got := h.googleRequests(); got != requests {
		t.Fatalf("requests after the mismatch = %d, want %d", got, requests)
	}
	if got := h.metrics.refreshFailures("wrong_account"); got != 1 {
		t.Fatalf("wrong account failures = %d", got)
	}
	if strings.Count(h.logs.String(), "assistant_mail.account.mismatch") != 1 {
		t.Fatalf("logs:\n%s", h.logs.String())
	}
	h.expectQuietLogs()
}

func TestAnExpiredAccessTokenIsRefreshedOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	h.sim.ExpireAccessTokens()
	id := h.deliver("assistant+a1@example.com", googlesim.Message{})
	h.poll()
	h.expectRoutes(id, "alice")
	refreshes := 0
	for _, request := range h.sim.Requests() {
		if request.Path == "/token" && request.Query.Get("grant_type") == "refresh_token" {
			refreshes++
		}
	}
	// One for the first poll, one after the 401.
	if refreshes != 2 {
		t.Fatalf("refreshes = %d", refreshes)
	}
}

func TestEverySwitchOffMakesNoGoogleRequests(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	for _, principal := range []string{"alice", "bob"} {
		if err := h.switches.SetEnabled(h.ctx, principal, assistantmail.Switch, false); err != nil {
			t.Fatal(err)
		}
	}
	h.deliver("assistant+a1@example.com", googlesim.Message{})
	requests := h.googleRequests()
	h.poll()
	if got := h.googleRequests(); got != requests {
		t.Fatalf("requests while paused = %d, want %d", got, requests)
	}
	if got := h.metrics.polls("paused"); got != 1 {
		t.Fatalf("paused polls = %d", got)
	}

	if err := h.switches.SetEnabled(h.ctx, "bob", assistantmail.Switch, true); err != nil {
		t.Fatal(err)
	}
	h.poll()
	if got := h.metrics.count("routed"); got != 1 {
		t.Fatalf("routed after a switch came on = %d", got)
	}
}

func TestAMalformedHeaderDoesNotBlockOtherMessages(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	junk := h.sim.DeliverTo(mailbox, googlesim.Message{Headers: []googlesim.Header{
		{Name: "Delivered-To", Value: mailbox},
		{Name: "From", Value: "=?x-unknown?q?broken?= <<<"},
		{Name: "To", Value: `"unterminated <assistant+a1@example.com`},
		{Name: "Cc", Value: "@@@, ,,; assistant+b1@example.com (comment"},
		{Name: "Subject", Value: "Junk"},
	}})
	salvaged := h.sim.DeliverTo(mailbox, googlesim.Message{Headers: []googlesim.Header{
		{Name: "Delivered-To", Value: mailbox},
		{Name: "From", Value: "orders@shop.example.org"},
		{Name: "To", Value: "=?x-unknown?q?Assistant?= <assistant+b1@example.com>, <<broken"},
		{Name: "Subject", Value: "Partly readable"},
	}})
	gone := h.deliver("assistant+a1@example.com", googlesim.Message{})
	h.sim.FailMessage(gone, 404)
	fine := h.deliver("assistant+a1@example.com", googlesim.Message{})
	h.poll()

	h.expectRoutes(junk)
	h.expectRoutes(salvaged, "bob")
	h.expectRoutes(gone)
	h.expectRoutes(fine, "alice")
	if fresh, err := h.store.RecordSeen(h.ctx, gone, time.Time{}, nil); err != nil || fresh {
		t.Fatalf("the unreadable message was not recorded as seen: %v %v", fresh, err)
	}
	h.expectQuietLogs()
}

func TestAFailedMessageReadFailsThePollAndKeepsTheCursor(t *testing.T) {
	t.Parallel()
	for _, status := range []int{400, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.start()
			id := h.deliver("assistant+a1@example.com", googlesim.Message{})
			h.sim.FailMessage(id, status)
			before := h.cursor()
			if err := h.service.Poll(h.ctx); err == nil {
				t.Fatal("the poll succeeded")
			}
			if h.cursor() != before {
				t.Fatal("the cursor moved past a message that was not recorded")
			}
			if got := h.metrics.polls("failed"); got != 1 {
				t.Fatalf("failed polls = %d", got)
			}

			h.sim.FailMessage(id, 0)
			h.poll()
			h.expectRoutes(id, "alice")
		})
	}
}

func TestAMessageThatFailsThreePollsIsSetAside(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	poison := h.deliver("assistant+a1@example.com", googlesim.Message{})
	h.sim.FailMessage(poison, 500)
	after := h.deliver("assistant+b1@example.com", googlesim.Message{})

	for range 2 {
		if err := h.service.Poll(h.ctx); err == nil {
			t.Fatal("the poll succeeded")
		}
		h.expectRoutes(after)
	}
	h.poll()
	h.expectRoutes(poison)
	h.expectRoutes(after, "bob")
	if fresh, err := h.store.RecordSeen(h.ctx, poison, time.Time{}, nil); err != nil || fresh {
		t.Fatalf("the unreadable message was not recorded as seen: %v %v", fresh, err)
	}
	if got := h.metrics.count("unreadable"); got != 1 {
		t.Fatalf("unreadable = %d", got)
	}
	if !strings.Contains(h.logs.String(), "assistant_mail.message.unreadable gmail_id="+poison) {
		t.Fatalf("logs:\n%s", h.logs.String())
	}
	h.expectQuietLogs()
}

func TestGmailAddressesMatchWithoutDotsAndOnEitherDomain(t *testing.T) {
	t.Parallel()
	h := newHarnessAt(t, "assistant@gmail.com")
	h.start()
	dotted := h.deliver("assistant@gmail.com", googlesim.Message{To: "As.Sis.Tant+a1+promo@GoogleMail.com"})
	plain := h.deliver("assistant@gmail.com", googlesim.Message{To: "assistant+b1@gmail.com"})
	other := h.deliver("assistant@gmail.com", googlesim.Message{To: "assistant+b1@gmail.co"})
	h.poll()

	h.expectRoutes(dotted, "alice")
	h.expectRoutes(plain, "bob")
	h.expectRoutes(other)
}

func TestOtherDomainsMatchExactlyAndTheTagEndsAtTheNextPlus(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	dotted := h.deliver(mailbox, googlesim.Message{To: "assis.tant+a1@example.com"})
	extra := h.deliver(mailbox, googlesim.Message{To: "assistant+b1+newsletter@example.com"})
	h.poll()

	h.expectRoutes(dotted)
	h.expectRoutes(extra, "bob")
}

func TestAGroupWithAnUnknownCharsetStillRoutes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	id := h.deliver(mailbox, googlesim.Message{To: "Team: assistant+a1@example.com, =?windows-1252?q?Caf=E9?= <cafe@example.org>;"})
	h.poll()
	h.expectRoutes(id, "alice")
}

func TestAPollCutOffByShutdownRecordsNoOutcome(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	h.deliver("assistant+a1@example.com", googlesim.Message{})
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	if err := h.service.Poll(ctx); err == nil {
		t.Fatal("a cancelled poll succeeded")
	}
	for _, outcome := range []string{"failed", "polled", "unavailable"} {
		if got := h.metrics.polls(outcome); got != 0 {
			t.Fatalf("%s polls = %d", outcome, got)
		}
	}
}

func TestTheLoopPollsUntilShutdown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(cfg *assistantmail.Config) {
		cfg.Jitter = func() time.Duration { return time.Millisecond }
	})
	if err := h.service.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.service.Start(h.ctx); err == nil {
		t.Fatal("a second loop started")
	}
	if err := h.service.Ready(h.ctx); err != nil {
		t.Fatal(err)
	}
	if outcome := h.waitPoll(); outcome != "started" {
		t.Fatalf("first loop poll = %q", outcome)
	}
	h.shutdown(time.Second)
	if err := h.service.Ready(h.ctx); err == nil {
		t.Fatal("ready after shutdown")
	}

	id := h.deliver("assistant+a1@example.com", googlesim.Message{})
	if err := h.service.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	if outcome := h.waitPoll(); outcome != "polled" {
		t.Fatalf("poll after a restart = %q", outcome)
	}
	h.shutdown(time.Second)
	h.expectRoutes(id, "alice")
}

func TestStartWaitsForALoopATimedOutShutdownLeftRunning(t *testing.T) {
	t.Parallel()
	stuck := &stuckSwitches{entered: make(chan struct{}, 1), release: make(chan struct{})}
	h := newHarness(t, func(cfg *assistantmail.Config) {
		stuck.reader = cfg.Switches
		cfg.Switches = stuck
		cfg.Jitter = func() time.Duration { return time.Millisecond }
	})
	if err := h.service.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	<-stuck.entered
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Millisecond)
	defer cancel()
	if err := h.service.Shutdown(ctx); err == nil {
		t.Fatal("shutdown finished while the loop was stuck")
	}
	if err := h.service.Start(h.ctx); err == nil {
		t.Fatal("a second loop started beside the stuck one")
	}

	close(stuck.release)
	h.shutdown(time.Second)
	if err := h.service.Start(h.ctx); err != nil {
		t.Fatalf("start after the old loop ended: %v", err)
	}
	h.shutdown(time.Second)
}

func (h *harness) waitPoll() string {
	h.t.Helper()
	select {
	case outcome := <-h.metrics.polled:
		return outcome
	case <-time.After(5 * time.Second):
		h.t.Fatal("the loop did not poll")
		return ""
	}
}

func (h *harness) shutdown(budget time.Duration) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := h.service.Shutdown(ctx); err != nil {
		h.t.Fatal(err)
	}
}

// stuckSwitches holds the first switch read until released, as a stuck poll would.
type stuckSwitches struct {
	reader  integration.SwitchReader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *stuckSwitches) Enabled(ctx context.Context, principalID string, sw integration.Switch) (bool, error) {
	s.once.Do(func() {
		s.entered <- struct{}{}
		<-s.release
	})
	return s.reader.Enabled(ctx, principalID, sw)
}

type metrics struct {
	mu        sync.Mutex
	pollsBy   map[string]int
	results   map[string]int
	failures  map[string]int
	noticesBy map[string]int
	triagesBy map[string]int
	routineBy map[string]int
	// polled hears each poll's outcome, for a test that waits on the loop.
	polled chan string
}

func (m *metrics) RecordAssistantMailPoll(_ context.Context, outcome string) {
	m.add(&m.pollsBy, outcome)
	select {
	case m.polled <- outcome:
	default:
	}
}

func (m *metrics) RecordAssistantMailMessage(_ context.Context, result string) {
	m.add(&m.results, result)
}

func (m *metrics) RecordAssistantMailRefreshFailure(_ context.Context, reason string) {
	m.add(&m.failures, reason)
}

func (m *metrics) RecordAssistantMailNotice(_ context.Context, outcome string) {
	m.add(&m.noticesBy, outcome)
}

func (m *metrics) RecordAssistantMailTriage(_ context.Context, outcome string) {
	m.add(&m.triagesBy, outcome)
}

func (m *metrics) RecordAssistantMailRoutineReply(_ context.Context, outcome string) {
	m.add(&m.routineBy, outcome)
}

func (m *metrics) routine(outcome string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routineBy[outcome]
}

func (m *metrics) triages(outcome string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.triagesBy[outcome]
}

func (m *metrics) notices(outcome string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.noticesBy[outcome]
}

func (m *metrics) add(counts *map[string]int, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if *counts == nil {
		*counts = make(map[string]int)
	}
	(*counts)[key]++
}

func (m *metrics) count(result string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.results[result]
}

func (m *metrics) polls(outcome string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pollsBy[outcome]
}

func (m *metrics) refreshFailures(reason string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failures[reason]
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
