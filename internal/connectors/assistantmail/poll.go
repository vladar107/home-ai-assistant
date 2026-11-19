package assistantmail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
)

const (
	// recoveryOverlap reaches back before the point an expired history cursor is recovered from,
	// since Gmail's internal date need not follow the order messages were added in.
	recoveryOverlap = time.Hour
	listPageSize    = 100
	// unreadableAfter is how many polls in a row may fail on one message before it is set aside.
	unreadableAfter = 3
)

// Message results, as counted.
const (
	resultRouted     = "routed"
	resultUnrouted   = "unrouted"
	resultSpam       = "spam"
	resultSkipped    = "skipped"
	resultSent       = "sent"
	resultUnreadable = "unreadable"
)

var routingHeaders = []string{"Delivered-To", "To", "Cc"}

// addressParser reads a display name in a charset Go does not know as its raw bytes, so such a
// name never costs the addresses of the whole header; only the addresses are used.
var addressParser = &mail.AddressParser{WordDecoder: &mime.WordDecoder{
	CharsetReader: func(_ string, input io.Reader) (io.Reader, error) { return input, nil },
}}

// Poll reads what was added to the mailbox since the stored history cursor, records each new
// message with its routes, and then tells each principal with the switch on of their new mail. It
// reads nothing while no principal with a tag has the switch on, though it still notes the
// switches, and fails with ErrUnavailable once Google rejected the refresh token. The loop calls
// it on every tick; it is safe to call directly.
func (s *Service) Poll(ctx context.Context) error {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	if !s.Available() {
		s.telemetry.RecordAssistantMailPoll(ctx, "unavailable")
		return ErrUnavailable
	}
	active, err := s.observeSwitches(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.telemetry.RecordAssistantMailPoll(ctx, "failed")
		}
		return fmt.Errorf("read assistant mailbox switches: %w", err)
	}
	if !active {
		s.telemetry.RecordAssistantMailPoll(ctx, "paused")
		return nil
	}
	outcome, err := s.poll(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		// A poll cut off by shutdown says nothing about the mailbox.
		return err
	case errors.Is(err, ErrUnavailable):
		outcome = "unavailable"
	case err != nil:
		outcome = "failed"
	}
	s.telemetry.RecordAssistantMailPoll(ctx, outcome)
	// Mail a poll recorded before it failed is still announced; once the mailbox is unavailable,
	// nothing can be read to announce it.
	if !errors.Is(err, ErrUnavailable) {
		s.announce(ctx)
	}
	return err
}

func (s *Service) poll(ctx context.Context) (string, error) {
	cursor, found, err := s.repository.HistoryCursor(ctx)
	if err != nil {
		return "", err
	}
	if !found {
		// A first poll starts from now; mail that arrived before the mailbox was connected is left
		// in Gmail.
		return "started", s.resetCursor(ctx)
	}
	if !s.accountChecked.Load() {
		if _, err := s.profile(ctx); err != nil {
			return "", err
		}
	}
	var added []string
	listed := make(map[string]bool)
	latest, pageToken := "", ""
	for {
		var page gmail.HistoryPage
		err := s.call(ctx, func(access string) error {
			var err error
			page, err = s.gmail.History(ctx, access, cursor, pageToken)
			return err
		})
		if errors.Is(err, gmail.ErrHistoryExpired) {
			return "recovered", s.recover(ctx)
		}
		if err != nil {
			return "", err
		}
		for _, ref := range page.Added {
			if ref.ID != "" && !listed[ref.ID] {
				listed[ref.ID] = true
				added = append(added, ref.ID)
			}
		}
		latest = page.HistoryID
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	for _, id := range added {
		if err := s.process(ctx, id); err != nil {
			return "", err
		}
	}
	if latest != "" && latest != cursor {
		if err := s.repository.SaveHistoryCursor(ctx, latest); err != nil {
			return "", err
		}
	}
	if !s.relistAfter.IsZero() {
		// Gmail's search can index a message some time after it arrived, so the window of the last
		// recovery is listed once more.
		if err := s.listSince(ctx, s.relistAfter); err != nil {
			return "", err
		}
		s.relistAfter = time.Time{}
	}
	return "polled", nil
}

// resetCursor starts the history cursor from the mailbox's current history id.
func (s *Service) resetCursor(ctx context.Context) error {
	historyID, err := s.currentHistoryID(ctx)
	if err != nil {
		return err
	}
	return s.repository.SaveHistoryCursor(ctx, historyID)
}

func (s *Service) currentHistoryID(ctx context.Context) (string, error) {
	profile, err := s.profile(ctx)
	if err != nil {
		return "", err
	}
	if profile.HistoryID == "" {
		return "", fmt.Errorf("assistant mailbox profile has no history id")
	}
	return profile.HistoryID, nil
}

// profile reads the mailbox's profile. The first read after start also checks that the refresh
// token reads the configured address: a token the operator authorized for another account makes
// the mailbox unavailable before any of that account's mail is read.
func (s *Service) profile(ctx context.Context) (gmail.Profile, error) {
	var profile gmail.Profile
	err := s.call(ctx, func(access string) error {
		var err error
		profile, err = s.gmail.Profile(ctx, access)
		return err
	})
	if err != nil {
		return gmail.Profile{}, err
	}
	if !s.accountChecked.Load() {
		local, domain, _ := strings.Cut(strings.ToLower(strings.TrimSpace(profile.EmailAddress)), "@")
		if gmailBase(local, domain) != s.base {
			s.wrongAccount.Store(true)
			s.telemetry.RecordAssistantMailRefreshFailure(ctx, "wrong_account")
			s.logger.ErrorContext(ctx, "assistant_mail.account.mismatch")
			return gmail.Profile{}, ErrUnavailable
		}
		s.accountChecked.Store(true)
	}
	return profile, nil
}

// recover replaces an expired history cursor: it lists every message since a little before the
// newest one seen, or before the cursor was last saved when none was, records those not seen yet,
// and restarts from the mailbox's current history id. The id is read first, so a message that
// arrives while the list is read is in the history after it. The next poll lists the same window
// again.
func (s *Service) recover(ctx context.Context) error {
	historyID, err := s.currentHistoryID(ctx)
	if err != nil {
		return err
	}
	since, err := s.recoveryStart(ctx)
	if err != nil {
		return err
	}
	if err := s.listSince(ctx, since); err != nil {
		return err
	}
	if err := s.repository.SaveHistoryCursor(ctx, historyID); err != nil {
		return err
	}
	s.relistAfter = since
	s.logger.InfoContext(ctx, "assistant_mail.cursor.recovered")
	return nil
}

func (s *Service) recoveryStart(ctx context.Context) (time.Time, error) {
	newest, found, err := s.repository.NewestSeenInternalDate(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if !found || newest.IsZero() {
		if newest, found, err = s.repository.HistoryCursorSavedAt(ctx); err != nil {
			return time.Time{}, err
		}
		if !found {
			return time.Time{}, fmt.Errorf("assistant mailbox history cursor is missing")
		}
	}
	return newest.Add(-recoveryOverlap), nil
}

// listSince lists every message Gmail's search finds from the time on, in any label, and records
// those not seen yet.
func (s *Service) listSince(ctx context.Context, since time.Time) error {
	query := "after:" + strconv.FormatInt(since.Unix(), 10) + " in:anywhere"
	var ids []string
	listed := make(map[string]bool)
	pageToken := ""
	for {
		var list gmail.MessageList
		err := s.call(ctx, func(access string) error {
			var err error
			list, err = s.gmail.ListMessagesPage(ctx, access, query, pageToken, listPageSize)
			return err
		})
		if err != nil {
			return err
		}
		for _, message := range list.Messages {
			if message.ID != "" && !listed[message.ID] {
				listed[message.ID] = true
				ids = append(ids, message.ID)
			}
		}
		if list.NextPageToken == "" {
			break
		}
		pageToken = list.NextPageToken
	}
	for _, id := range ids {
		if err := s.process(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// process records one message as seen, with a route to each principal it belongs to. A message
// Gmail no longer has, or one that failed several polls in a row, is recorded without routes so
// it never holds up the rest; any other failure fails the poll, which reads it again next time.
func (s *Service) process(ctx context.Context, id string) error {
	seen, err := s.repository.Seen(ctx, id)
	if err != nil || seen {
		return err
	}
	var message gmail.Message
	err = s.call(ctx, func(access string) error {
		var err error
		message, err = s.gmail.MessageHeaders(ctx, access, id, gmail.InboundHeaders)
		return err
	})
	if err != nil {
		return s.readFailed(ctx, id, err)
	}
	delete(s.failures, id)
	millis, err := strconv.ParseInt(message.InternalDate, 10, 64)
	if err != nil {
		s.logger.WarnContext(ctx, "assistant_mail.message.malformed", "gmail_id", id, "reason", "internal_date")
		return s.record(ctx, id, time.Time{}, nil, resultSkipped)
	}
	internalDate := time.UnixMilli(millis)
	labels := message.LabelIDs
	switch {
	case slices.Contains(labels, "TRASH") || slices.Contains(labels, "DRAFT"):
		return s.record(ctx, id, internalDate, nil, resultSkipped)
	case slices.Contains(labels, "SENT") && !slices.Contains(labels, "INBOX"):
		// The send path records its own outgoing route. One it failed to record is recovered from
		// the principal's tag in Reply-To, so replies to the plain address still find them.
		principal, found := s.sentFor(message)
		if !found {
			return s.record(ctx, id, internalDate, nil, resultSkipped)
		}
		now := s.now()
		route := Route{
			GmailID: id, PrincipalID: principal, ThreadID: message.ThreadID, Direction: DirectionOut,
			InternalDate: internalDate, RecordedAt: now, NotifiedAt: now,
		}
		return s.record(ctx, id, internalDate, []Route{route}, resultSent)
	}
	principals := s.tagPrincipals(message)
	if len(principals) == 0 && message.ThreadID != "" {
		if principals, err = s.repository.PrincipalsForThread(ctx, message.ThreadID); err != nil {
			return err
		}
	}
	if len(principals) == 0 {
		return s.record(ctx, id, internalDate, nil, resultUnrouted)
	}
	spam := slices.Contains(labels, "SPAM")
	now := s.now()
	routes := make([]Route, 0, len(principals))
	for _, principal := range principals {
		route := Route{
			GmailID: id, PrincipalID: principal, ThreadID: message.ThreadID, Direction: DirectionIn,
			InternalDate: internalDate, Spam: spam, RecordedAt: now,
		}
		// Mail for a principal with the switch off, or from before they last turned it on, is
		// never announced, even once the switch is on.
		if state := s.observed[principal]; !state.Enabled || internalDate.Before(state.OnSince) {
			route.NotifiedAt = now
		}
		routes = append(routes, route)
	}
	result := resultRouted
	if spam {
		result = resultSpam
	}
	return s.record(ctx, id, internalDate, routes, result)
}

// readFailed handles a message whose headers could not be read. A message Gmail no longer has is
// recorded at once; one Gmail fails on itself, with a 400 or a 5xx, is recorded after it failed
// several polls in a row. A rate limit, an authorization problem or a broken connection says
// nothing about the message and only fails the poll.
func (s *Service) readFailed(ctx context.Context, id string, err error) error {
	var googleErr *google.Error
	if !errors.As(err, &googleErr) || ctx.Err() != nil {
		return err
	}
	switch {
	case googleErr.Class == google.ErrorNotFound:
		delete(s.failures, id)
		s.logger.InfoContext(ctx, "assistant_mail.message.gone", "gmail_id", id)
		return s.record(ctx, id, time.Time{}, nil, resultSkipped)
	case googleErr.Class == google.ErrorInvalid || (googleErr.Class == google.ErrorUnavailable && googleErr.Status != 0):
		s.failures[id]++
		if s.failures[id] < unreadableAfter {
			return err
		}
		delete(s.failures, id)
		s.logger.WarnContext(ctx, "assistant_mail.message.unreadable", "gmail_id", id)
		return s.record(ctx, id, time.Time{}, nil, resultUnreadable)
	}
	return err
}

// record stores the message as seen and counts it, once.
func (s *Service) record(ctx context.Context, id string, internalDate time.Time, routes []Route, result string) error {
	fresh, err := s.repository.RecordSeen(ctx, id, internalDate, routes)
	if err != nil {
		return err
	}
	if fresh {
		s.telemetry.RecordAssistantMailMessage(ctx, result)
	}
	return nil
}

// tagPrincipals returns the principals whose tags, current or retired, the message was addressed
// to on the mailbox's own address, in Delivered-To, To and Cc together.
func (s *Service) tagPrincipals(message gmail.Message) []string {
	var principals []string
	for _, name := range routingHeaders {
		for _, value := range message.HeaderValues(name) {
			for _, address := range addresses(value) {
				tag, ok := s.tagOf(address)
				if !ok {
					continue
				}
				if principal, found := s.mailbox.PrincipalForTag(tag); found && !slices.Contains(principals, principal) {
					principals = append(principals, principal)
				}
			}
		}
	}
	slices.Sort(principals)
	return principals
}

// sentFor returns the principal a message the mailbox sent was written for, read from the tagged
// address in its Reply-To.
func (s *Service) sentFor(message gmail.Message) (string, bool) {
	for _, value := range message.HeaderValues("Reply-To") {
		for _, address := range addresses(value) {
			if tag, ok := s.tagOf(address); ok {
				if principal, found := s.mailbox.PrincipalForTag(tag); found {
					return principal, true
				}
			}
		}
	}
	return "", false
}

// tagOf returns the plus tag of an address on the mailbox's own address: the text after the
// first + up to the next one, with the rest of the address matched as Gmail delivers it.
func (s *Service) tagOf(address string) (string, bool) {
	address = strings.ToLower(strings.TrimSpace(address))
	at := strings.LastIndex(address, "@")
	if at < 0 {
		return "", false
	}
	base, rest, tagged := strings.Cut(address[:at], "+")
	tag, _, _ := strings.Cut(rest, "+")
	if !tagged || tag == "" || gmailBase(base, address[at+1:]) != s.base {
		return "", false
	}
	return tag, true
}

// gmailBase is the mailbox an address without its tag reaches: Gmail ignores dots in its own
// addresses and treats googlemail.com as gmail.com; any other domain is taken as written.
func gmailBase(local, domain string) string {
	if domain == "googlemail.com" {
		domain = "gmail.com"
	}
	if domain == "gmail.com" {
		local = strings.ReplaceAll(local, ".", "")
	}
	return local + "@" + domain
}

// addresses reads the addresses of one header value. A value that cannot be read as a whole is
// read item by item, and an item that cannot be read still counts when it is a bare address, so
// one malformed header loses as little as possible.
func addresses(value string) []string {
	if list, err := addressParser.ParseList(value); err == nil {
		found := make([]string, 0, len(list))
		for _, address := range list {
			found = append(found, address.Address)
		}
		return found
	}
	var found []string
	for _, item := range strings.Split(value, ",") {
		if address, err := addressParser.Parse(item); err == nil {
			found = append(found, address.Address)
			continue
		}
		if start, end := strings.LastIndex(item, "<"), strings.LastIndex(item, ">"); start >= 0 && end > start {
			item = item[start+1 : end]
		}
		item = strings.TrimSpace(item)
		if strings.Count(item, "@") == 1 && !strings.ContainsAny(item, " \t\"<>") {
			found = append(found, item)
		}
	}
	return found
}
