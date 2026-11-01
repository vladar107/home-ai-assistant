// Package thread carries the Telegram tab a piece of work belongs to. A send made under a context
// with a thread lands in that tab; one made without lands in the chat's main view.
package thread

import (
	"context"
	"slices"
	"strings"
)

type contextKey struct{}

// deliveryKey holds the tab a delivery nobody asked for is sent to. It is apart from contextKey,
// which scopes a lane's attachments, approvals, and exchanges, so a scheduled run delivered into a
// feed tab never reads that tab's conversation.
type deliveryKey struct{}

// With returns a context whose sends go to the given tab; zero means the main view.
func With(ctx context.Context, threadID int64) context.Context {
	if threadID <= 0 {
		return context.WithValue(ctx, contextKey{}, int64(0))
	}
	return context.WithValue(ctx, contextKey{}, threadID)
}

// From returns the tab of the context, or zero for the main view.
func From(ctx context.Context) int64 {
	threadID, _ := ctx.Value(contextKey{}).(int64)
	return threadID
}

// WithDelivery returns a context whose sends go to the given tab, zero for the main view, whatever
// tab From names. Only the Telegram client reads it.
func WithDelivery(ctx context.Context, threadID int64) context.Context {
	return context.WithValue(ctx, deliveryKey{}, max(threadID, 0))
}

// Delivery returns the tab WithDelivery set, and whether it set one.
func Delivery(ctx context.Context) (int64, bool) {
	threadID, set := ctx.Value(deliveryKey{}).(int64)
	return threadID, set
}

// The feeds are the tabs that messages nobody asked for go to while the bot has tabs on, one per
// kind of message.
const (
	FeedReminders = "reminders"
	FeedReports   = "reports"
	FeedHome      = "home"
	FeedWatches   = "watches"
	FeedEvents    = "events"
	FeedInbox     = "inbox"
)

// FeedSubject is what the tab registry records a feed's tab as serving.
func FeedSubject(feed string) string {
	return "feed:" + feed
}

// The kinds of object whose messages go to a tab of their own. An object's destination is its kind
// and its ID, such as trip:<id>, and the tab registry records its tab as serving that destination.
const (
	ObjectTrip    = "trip"
	ObjectProject = "project"
)

// objectKinds are the kinds of object with a tab of their own, in a fixed order.
var objectKinds = []string{ObjectTrip, ObjectProject}

// ObjectKinds returns the kinds of object with a tab of their own.
func ObjectKinds() []string { return slices.Clone(objectKinds) }

// ObjectDestination is where an object's messages go: its own tab.
func ObjectDestination(kind, id string) string {
	return kind + ":" + id
}

// ObjectOf returns the object a destination names, if it names one of a kind with a tab of its own.
func ObjectOf(destination string) (kind, id string, found bool) {
	kind, id, cut := strings.Cut(destination, ":")
	if !cut || id == "" || !slices.Contains(objectKinds, kind) {
		return "", "", false
	}
	return kind, id, true
}

// TripDestination is where a trip's messages go: the trip's own tab, which the tab registry records
// as serving the same value.
func TripDestination(tripID string) string {
	return ObjectDestination(ObjectTrip, tripID)
}

// TripOf returns the trip a destination names, if it names one, and "" otherwise.
func TripOf(destination string) (string, bool) {
	return objectOfKind(destination, ObjectTrip)
}

// ProjectDestination is where a project's messages go: the project's own tab.
func ProjectDestination(projectID string) string {
	return ObjectDestination(ObjectProject, projectID)
}

// ProjectOf returns the project a destination names, if it names one, and "" otherwise.
func ProjectOf(destination string) (string, bool) {
	return objectOfKind(destination, ObjectProject)
}

func objectOfKind(destination, wanted string) (string, bool) {
	kind, id, found := ObjectOf(destination)
	if !found || kind != wanted {
		return "", false
	}
	return id, true
}

// Feeds resolves the tab of a principal's feed, making it on first use. Zero is the main view,
// which is where everything goes while the bot has tabs off.
type Feeds interface {
	Thread(ctx context.Context, principalID string, chatID int64, feed string) (int64, error)
	// Destination resolves a destination such as TripDestination or ProjectDestination, and the feed when the
	// destination has no tab to offer.
	Destination(ctx context.Context, principalID string, chatID int64, destination, feed string) (int64, error)
}
