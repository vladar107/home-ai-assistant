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

// Route is where a delivery nobody asked for goes, such as a scheduled run's report: a tab found
// the first time a send needs it and then kept, so a delivery that sends nothing makes no tab.
type Route interface {
	// Tab returns the route's tab, zero for the main view, finding it at the first call under the
	// sending context.
	Tab(ctx context.Context) int64
	// Peek returns the tab the route would use without making one, and false when finding it would
	// make one.
	Peek(ctx context.Context) (int64, bool)
	// Resolved returns the route's tab once it was found, and the main view before.
	Resolved() int64
	// Replace is told that Telegram refused gone, the tab a send of the route named, and that it was
	// taken for gone, with Telegram's error. It returns the tab the route sends to from now on, and
	// whether a send may be made there again now; a route finds one replacement at most, and then
	// sends to the main view.
	Replace(ctx context.Context, gone int64, cause error) (int64, bool)
}

// WithRoute returns a context whose sends go where the route finds, whatever tab From names. Like
// WithDelivery, it replaces any delivery the context had, and a later WithDelivery replaces it.
func WithRoute(ctx context.Context, route Route) context.Context {
	return context.WithValue(ctx, deliveryKey{}, route)
}

// Delivery returns the tab WithDelivery set, or the one its route finds, and whether either set
// one.
func Delivery(ctx context.Context) (int64, bool) {
	switch delivery := ctx.Value(deliveryKey{}).(type) {
	case int64:
		return delivery, true
	case Route:
		return delivery.Tab(ctx), true
	}
	return 0, false
}

// Peek is Delivery without making a tab: it reports false when the context's route would have to
// make one.
func Peek(ctx context.Context) (int64, bool) {
	if route, set := ctx.Value(deliveryKey{}).(Route); set {
		return route.Peek(ctx)
	}
	threadID, _ := Delivery(ctx)
	return threadID, true
}

// Resolved is Delivery without finding a tab: a route not yet resolved is the main view.
func Resolved(ctx context.Context) (int64, bool) {
	if route, set := ctx.Value(deliveryKey{}).(Route); set {
		return route.Resolved(), true
	}
	return Delivery(ctx)
}

// RouteOf returns the context's route, when its delivery is one.
func RouteOf(ctx context.Context) (Route, bool) {
	route, set := ctx.Value(deliveryKey{}).(Route)
	return route, set
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
	FeedParcels   = "parcels"
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
	// ObjectWatch is an agent watch. Its destination is the subject a conversation about any watch
	// has, so a watch of another kind has that subject and no tab.
	ObjectWatch = "watch"
)

// objectKinds are the kinds of object with a tab of their own, in a fixed order.
var objectKinds = []string{ObjectTrip, ObjectProject, ObjectWatch}

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

// WatchDestination is where an agent watch's messages go: the watch's own tab.
func WatchDestination(watchID string) string {
	return ObjectDestination(ObjectWatch, watchID)
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
	// Destination resolves a destination such as TripDestination, ProjectDestination, or
	// WatchDestination, and the feed when the destination has no tab to offer.
	Destination(ctx context.Context, principalID string, chatID int64, destination, feed string) (int64, error)
	// Route is a delivery's Destination, or with no destination its feed, found at its first send.
	// A tab it makes is named in the language of ctx.
	Route(ctx context.Context, principalID string, chatID int64, destination, feed string) Route
}

// FixedRoute is a route to a tab known in advance, zero for the main view.
type FixedRoute int64

func (r FixedRoute) Tab(context.Context) int64 { return max(int64(r), 0) }

func (r FixedRoute) Peek(context.Context) (int64, bool) { return max(int64(r), 0), true }

func (r FixedRoute) Resolved() int64 { return max(int64(r), 0) }

// Replace finds no replacement: a fixed tab stays what it is.
func (r FixedRoute) Replace(context.Context, int64, error) (int64, bool) {
	return max(int64(r), 0), false
}
