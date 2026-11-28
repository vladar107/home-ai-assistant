package gmail

import (
	"context"
	"errors"
	"net/mail"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
)

// ErrMessageGone means the message was deleted since a search found it.
var ErrMessageGone = errors.New("the message is no longer in the mailbox")

// AccountWide reports whether a mail error would fail every message of the mailbox, as a rejected
// grant or a rate limit does, rather than one message.
func AccountWide(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, google.ErrNotConnected) {
		return true
	}
	class := errorClass(err)
	return class == google.ErrorUnauthorized || class == google.ErrorRateLimited
}

// MailRef names one message a search found.
type MailRef struct {
	Account  string
	ID       string
	Received time.Time
}

// Mail is one message as code reads it: the sender's address, the subject, when Gmail received it,
// and its text without links.
type Mail struct {
	MailRef
	From    string
	Subject string
	Text    string
}

var mailLink = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)

// SearchMail searches every mailbox the principal can read with Gmail's search syntax and returns
// the newest limit matches, oldest first. A mailbox that cannot be searched fails the call, so a
// caller that moves on after a search never skips mail it did not see. It is read-only.
func (s *Service) SearchMail(ctx context.Context, principalID, query string, limit int) ([]MailRef, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > maximumQueryRunes || limit <= 0 {
		return nil, errors.New("the mail search is not valid")
	}
	accounts, err := s.grants.Accounts(ctx, principalID, google.ProductMail)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, google.ErrNotConnected
	}
	var found []MailRef
	for _, account := range accounts {
		var result mailboxSearch
		err := s.withAccess(ctx, principalID, account, func(access string) (err error) {
			result, err = s.search(ctx, access, query, limit)
			return err
		})
		if err == nil && result.skipped > 0 {
			err = result.reason
		}
		if err != nil {
			s.logger.InfoContext(ctx, "gmail.mail_search.failed", "principal_id", principalID, "error_class", errorClass(err))
			return nil, err
		}
		for _, message := range result.messages {
			millis, err := strconv.ParseInt(message.InternalDate, 10, 64)
			if err != nil || millis <= 0 {
				continue
			}
			found = append(found, MailRef{Account: account.Address, ID: message.ID, Received: time.UnixMilli(millis)})
		}
	}
	slices.SortStableFunc(found, func(a, b MailRef) int { return a.Received.Compare(b.Received) })
	if len(found) > limit {
		found = found[len(found)-limit:]
	}
	return found, nil
}

// ThreadMessage is one message of a thread as code lists it: when Gmail received it, and whether
// the mailbox's owner sent it.
type ThreadMessage struct {
	ID       string
	Received time.Time
	Sent     bool
}

// ListThread lists the messages of one thread in one of the principal's mailboxes from their
// metadata, without reading any text. It is read-only.
func (s *Service) ListThread(ctx context.Context, principalID, address, threadID string) ([]ThreadMessage, error) {
	accounts, err := s.grants.Accounts(ctx, principalID, google.ProductMail)
	if err != nil {
		return nil, err
	}
	account, found := findAccount(accounts, address)
	if !found {
		return nil, google.ErrNotConnected
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" || len(threadID) > 64 || strings.ContainsAny(threadID, "/?#") {
		return nil, errors.New("the thread id is not valid")
	}
	var messages []Message
	err = s.withAccess(ctx, principalID, account, func(access string) (err error) {
		messages, err = s.client.ThreadMetadata(ctx, access, threadID, []string{"Date"})
		return err
	})
	if err != nil {
		s.logger.InfoContext(ctx, "gmail.thread_list.failed", "principal_id", principalID, "error_class", errorClass(err))
		return nil, err
	}
	listed := make([]ThreadMessage, 0, len(messages))
	for _, message := range messages {
		millis, err := strconv.ParseInt(message.InternalDate, 10, 64)
		if err != nil || millis <= 0 {
			continue
		}
		listed = append(listed, ThreadMessage{ID: message.ID, Received: time.UnixMilli(millis), Sent: slices.Contains(message.LabelIDs, "SENT")})
	}
	return listed, nil
}

// ReadMail reads one message a search found, or returns ErrMessageGone when it was deleted since.
func (s *Service) ReadMail(ctx context.Context, principalID string, ref MailRef) (Mail, error) {
	accounts, err := s.grants.Accounts(ctx, principalID, google.ProductMail)
	if err != nil {
		return Mail{}, err
	}
	account, found := findAccount(accounts, ref.Account)
	if !found {
		return Mail{}, google.ErrNotConnected
	}
	id := strings.TrimSpace(ref.ID)
	if id == "" || len(id) > 64 || strings.ContainsAny(id, "/?#") {
		return Mail{}, errors.New("the message id is not valid")
	}
	var message Message
	err = s.withAccess(ctx, principalID, account, func(access string) (err error) {
		message, err = s.client.MessageFull(ctx, access, id)
		return err
	})
	if err != nil {
		if errorClass(err) == google.ErrorNotFound {
			return Mail{}, ErrMessageGone
		}
		s.logger.InfoContext(ctx, "gmail.mail_read.failed", "principal_id", principalID, "error_class", errorClass(err))
		return Mail{}, err
	}
	read := MailRef{Account: account.Address, ID: id, Received: ref.Received}
	if millis, err := strconv.ParseInt(message.InternalDate, 10, 64); err == nil && millis > 0 {
		read.Received = time.UnixMilli(millis)
	}
	from := header(message.Payload, "From")
	if parsed, err := mail.ParseAddress(from); err == nil {
		from = parsed.Address
	}
	text := messageText(message.Payload)
	text = strings.ToValidUTF8(wholeRunes(text[:min(len(text), maximumReadBytes)]), "")
	return Mail{
		MailRef: read, From: strings.ToLower(strings.TrimSpace(from)), Subject: header(message.Payload, "Subject"),
		Text: strings.TrimSpace(mailLink.ReplaceAllString(text, "")),
	}, nil
}
