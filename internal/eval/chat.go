package eval

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/project"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/trip"
	"github.com/vladar107/home-ai-assistant/internal/turn"
	"github.com/vladar107/home-ai-assistant/internal/watch"
	"github.com/vladar107/home-ai-assistant/internal/wiring"
)

const captionLimit = 1024

// Reply is one message the assistant sent as a reply.
type Reply struct {
	MessageID int64
	ReplyTo   int64
	Text      string
}

// Offer is one proposal message with its buttons: the bundle a Confirm or Reject acts on.
type Offer struct {
	MessageID int64
	Text      string
	Confirm   string
	Reject    string
}

// Card is a picture sent to the chat.
type Card struct {
	Caption string
	Bytes   int
}

// Reaction is an emoji the assistant set on a message.
type Reaction struct {
	MessageID int64
	Emoji     string
}

// Captured is everything the stub took since the last drain, in the order it arrived.
type Captured struct {
	Replies []Reply
	// Texts are the messages without buttons that a bundle sends before its last part.
	Texts     []string
	Offers    []Offer
	Direct    []string
	Cards     []Card
	Documents []string
	Reactions []Reaction
	// Notices are reminders, watch alerts, task messages and trip messages, which a chat turn does
	// not send.
	Notices []string
}

// Chat is the recording stub for everything the wiring needs from Telegram. It sends nothing
// anywhere.
type Chat struct {
	mu       sync.Mutex
	nextID   int64
	captured Captured
}

// Compile-time proof that the stub is what the wiring asks for.
var (
	_ wiring.Chat        = (*Chat)(nil)
	_ wiring.TripNotices = (*Chat)(nil)
)

func newChat() *Chat { return &Chat{nextID: 1000} }

// NextID returns a message ID from the one sequence the user's and the assistant's messages share,
// as a Telegram chat numbers them.
func (c *Chat) NextID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

// Drain returns what was captured since the last drain and starts over.
func (c *Chat) Drain() Captured {
	c.mu.Lock()
	defer c.mu.Unlock()
	taken := c.captured
	c.captured = Captured{}
	return taken
}

func (c *Chat) record(capture func(*Captured)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	capture(&c.captured)
}

func (c *Chat) reply(replyTo int64, text string) int64 {
	id := c.NextID()
	c.record(func(captured *Captured) {
		captured.Replies = append(captured.Replies, Reply{MessageID: id, ReplyTo: replyTo, Text: text})
	})
	return id
}

func (c *Chat) notice(text string) int64 {
	id := c.NextID()
	c.record(func(captured *Captured) { captured.Notices = append(captured.Notices, text) })
	return id
}

func (c *Chat) SendReply(_ context.Context, _, replyTo int64, text string) error {
	c.reply(replyTo, text)
	return nil
}

func (c *Chat) SendTrackedReply(_ context.Context, _, replyTo int64, text string) (int64, int64, error) {
	return c.reply(replyTo, text), 0, nil
}

func (c *Chat) SendTyping(context.Context, int64) error { return nil }

func (c *Chat) SendMessageDraft(context.Context, int64, int64, string, bool) error { return nil }

func (c *Chat) SendPhoto(_ context.Context, _, _ int64, png []byte, caption string) error {
	c.record(func(captured *Captured) {
		captured.Cards = append(captured.Cards, Card{Caption: caption, Bytes: len(png)})
	})
	return nil
}

func (c *Chat) CaptionFits(caption string) bool {
	return utf8.RuneCountInString(caption) <= captionLimit
}

func (c *Chat) SendMessage(_ context.Context, _ int64, text string) error {
	c.notice(text)
	return nil
}

func (c *Chat) SendDocument(_ context.Context, _ int64, name string, body io.Reader, _ int64) error {
	_, _ = io.Copy(io.Discard, body)
	c.record(func(captured *Captured) { captured.Documents = append(captured.Documents, name) })
	return nil
}

func (c *Chat) SetMessageReaction(_ context.Context, _, messageID int64, emoji string) error {
	c.record(func(captured *Captured) {
		captured.Reactions = append(captured.Reactions, Reaction{MessageID: messageID, Emoji: emoji})
	})
	return nil
}

func (c *Chat) SendText(_ context.Context, _ int64, text string) (int64, error) {
	id := c.NextID()
	c.record(func(captured *Captured) { captured.Texts = append(captured.Texts, text) })
	return id, nil
}

func (c *Chat) SendProposal(_ context.Context, _ int64, shown approval.Shown, buttons approval.Buttons) (int64, approval.WidgetKind, error) {
	id := c.NextID()
	c.record(func(captured *Captured) {
		captured.Offers = append(captured.Offers, Offer{MessageID: id, Text: shown.Text, Confirm: buttons.Confirm, Reject: buttons.Reject})
	})
	return id, buttons.Kind, nil
}

func (c *Chat) EditText(context.Context, int64, int64, approval.Shown) error { return nil }

func (c *Chat) EditProposal(context.Context, int64, int64, approval.Shown, approval.Buttons) error {
	return nil
}

func (c *Chat) SendPreview(_ context.Context, _, _ int64, text string) error {
	c.notice(text)
	return nil
}

func (c *Chat) SendDirectActions(_ context.Context, _ int64, actions approval.DirectActions) (int64, error) {
	id := c.NextID()
	c.record(func(captured *Captured) { captured.Direct = append(captured.Direct, actions.Text) })
	return id, nil
}

func (c *Chat) EditDirectActions(context.Context, int64, int64, approval.DirectActions) error {
	return nil
}

func (c *Chat) SendReminder(_ context.Context, _ int64, _, text string) (int64, int64, error) {
	return c.notice(text), 0, nil
}

func (c *Chat) SendAlert(_ context.Context, _ int64, _, text string) (int64, int64, error) {
	return c.notice(text), 0, nil
}

func (c *Chat) SendStatus(_ context.Context, _, _ int64, text, _ string) (int64, error) {
	return c.notice(text), nil
}

func (c *Chat) EditStatus(context.Context, int64, int64, string, string, bool) error { return nil }

func (c *Chat) Thread(context.Context, string, int64, string) (int64, error) { return 0, nil }

func (c *Chat) Destination(context.Context, string, int64, string, string) (int64, error) {
	return 0, nil
}

func (c *Chat) Route(context.Context, string, int64, string, string) thread.Route {
	return thread.FixedRoute(0)
}

func (c *Chat) TabReplied(context.Context, conversation.TabReply) {}

func (c *Chat) TripChanged(context.Context, trip.Trip) {}

func (c *Chat) CheckTab(context.Context, string, string, int64, bool) (trip.TabMove, error) {
	return trip.TabMove{}, nil
}

func (c *Chat) TakeTab(context.Context, trip.Trip, int64, bool) (trip.TabMove, error) {
	return trip.TabMove{}, nil
}

func (c *Chat) SendTripCard(_ context.Context, found trip.Trip) error {
	c.notice(found.Title)
	return nil
}

func (c *Chat) Send(_ context.Context, _, _, text string) error {
	c.notice(text)
	return nil
}

func (c *Chat) TripTab(context.Context, string, string) (int64, error) { return 0, nil }

// tabs is what the wiring builds the tab services from; the stub keeps no tabs.
func (c *Chat) tabs(*turn.Engine, trip.Reader, project.Reader, watch.Reader) wiring.TabServices {
	return wiring.TabServices{Feeds: c, Replies: c, Trips: c}
}

// text joins the reply chunks of one turn.
func (c Captured) text() string {
	parts := make([]string, 0, len(c.Replies))
	for _, reply := range c.Replies {
		parts = append(parts, reply.Text)
	}
	return strings.Join(parts, "\n")
}

func (c Captured) lastReplyID() int64 {
	if len(c.Replies) == 0 {
		return 0
	}
	return slices.MaxFunc(c.Replies, func(a, b Reply) int { return int(a.MessageID - b.MessageID) }).MessageID
}
