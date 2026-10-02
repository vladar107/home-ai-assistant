package bus

import (
	"context"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

const (
	alertsFile     = "alerts.pb"
	alertsLifetime = 2 * time.Minute
	// alertsRetryDelay keeps a failing feed from costing a request on every call.
	alertsRetryDelay = time.Minute
)

// GTFS-realtime field numbers of the parts of an alert the tools read.
const (
	feedEntity           protowire.Number = 2
	entityAlert          protowire.Number = 5
	alertActivePeriod    protowire.Number = 1
	alertInformedEntity  protowire.Number = 5
	alertEffect          protowire.Number = 7
	alertHeaderText      protowire.Number = 10
	alertDescriptionText protowire.Number = 11
	periodStart          protowire.Number = 1
	periodEnd            protowire.Number = 2
	selectorRouteID      protowire.Number = 2
	selectorStopID       protowire.Number = 5
	translatedText       protowire.Number = 1
	translationText      protowire.Number = 1
	translationLanguage  protowire.Number = 2
)

var effects = map[uint64]string{
	1:  "no service",
	2:  "reduced service",
	3:  "significant delays",
	4:  "detour",
	5:  "additional service",
	6:  "modified service",
	9:  "stop moved",
	11: "accessibility issue",
}

type period struct {
	start, end time.Time
}

// selector is one informed entity: a stop, a line, or a line at one stop.
type selector struct {
	stopID  string
	routeID string
}

type alert struct {
	effect      string
	header      string
	description string
	periods     []period
	selectors   []selector
}

// activeAt tells whether the alert covers at; an alert without periods is active until withdrawn.
func (a alert) activeAt(at time.Time) bool {
	if len(a.periods) == 0 {
		return true
	}
	for _, p := range a.periods {
		if (p.start.IsZero() || !at.Before(p.start)) && (p.end.IsZero() || at.Before(p.end)) {
			return true
		}
	}
	return false
}

// end is when the period covering at closes; zero when open-ended.
func (a alert) end(at time.Time) time.Time {
	for _, p := range a.periods {
		if (p.start.IsZero() || !at.Before(p.start)) && (p.end.IsZero() || at.Before(p.end)) {
			return p.end
		}
	}
	return time.Time{}
}

// alertFeed is the national alert list, fetched on demand and kept for a short while.
type alertFeed struct {
	fetch func(context.Context) ([]alert, error)
	now   func() time.Time

	mu         sync.Mutex
	list       []alert
	fetchedAt  time.Time
	retryAfter time.Time
	loaded     bool
}

// load returns the current alerts. A failed fetch is reported once per retry delay; meanwhile the
// last list, if any, keeps answering.
func (f *alertFeed) load(ctx context.Context) ([]alert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if f.loaded && (now.Sub(f.fetchedAt) < alertsLifetime || now.Before(f.retryAfter)) {
		return f.list, nil
	}
	list, err := f.fetch(ctx)
	if err != nil {
		f.retryAfter = now.Add(alertsRetryDelay)
		if f.loaded {
			return f.list, err
		}
		return nil, err
	}
	f.list, f.fetchedAt, f.retryAfter, f.loaded = list, now, time.Time{}, true
	return list, nil
}

func (c *client) alerts(ctx context.Context) ([]alert, error) {
	body, err := c.fetch(ctx, alertsFile, nil, maximumAPIBytes)
	if err != nil {
		return nil, err
	}
	return decodeAlerts(body)
}

// decodeAlerts reads the alerts out of a GTFS-realtime feed message, field by field, so no
// generated bindings are needed for the handful of fields used.
func decodeAlerts(body []byte) ([]alert, error) {
	var list []alert
	err := eachField(body, func(number protowire.Number, value []byte) error {
		if number != feedEntity {
			return nil
		}
		return eachField(value, func(number protowire.Number, value []byte) error {
			if number != entityAlert {
				return nil
			}
			item, err := decodeAlert(value)
			if err != nil {
				return err
			}
			list = append(list, item)
			return nil
		})
	})
	if err != nil {
		return nil, errMalformed
	}
	return list, nil
}

func decodeAlert(body []byte) (alert, error) {
	var item alert
	err := eachValue(body, func(number protowire.Number, value []byte, scalar uint64) error {
		switch number {
		case alertActivePeriod:
			var p period
			if err := eachValue(value, func(number protowire.Number, _ []byte, scalar uint64) error {
				switch number {
				case periodStart:
					p.start = unixTime(scalar)
				case periodEnd:
					p.end = unixTime(scalar)
				}
				return nil
			}); err != nil {
				return err
			}
			item.periods = append(item.periods, p)
		case alertInformedEntity:
			var entity selector
			if err := eachField(value, func(number protowire.Number, value []byte) error {
				switch number {
				case selectorStopID:
					entity.stopID = string(value)
				case selectorRouteID:
					entity.routeID = string(value)
				}
				return nil
			}); err != nil {
				return err
			}
			if entity != (selector{}) {
				item.selectors = append(item.selectors, entity)
			}
		case alertEffect:
			item.effect = effects[scalar]
		case alertHeaderText:
			text, err := translated(value)
			item.header = text
			return err
		case alertDescriptionText:
			text, err := translated(value)
			item.description = text
			return err
		}
		return nil
	})
	return item, err
}

// translated picks English when the alert offers it, and otherwise the first translation, which in
// the Dutch feed is Dutch.
func translated(body []byte) (string, error) {
	first, english := "", ""
	err := eachField(body, func(number protowire.Number, value []byte) error {
		if number != translatedText {
			return nil
		}
		var text, language string
		if err := eachField(value, func(number protowire.Number, value []byte) error {
			switch number {
			case translationText:
				text = string(value)
			case translationLanguage:
				language = string(value)
			}
			return nil
		}); err != nil {
			return err
		}
		if first == "" {
			first = text
		}
		if strings.EqualFold(language, "en") && english == "" {
			english = text
		}
		return nil
	})
	if english != "" {
		return english, err
	}
	return first, err
}

func unixTime(seconds uint64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	return time.Unix(int64(seconds), 0)
}

// eachField calls visit with every length-delimited field of a message and skips the rest.
func eachField(body []byte, visit func(protowire.Number, []byte) error) error {
	return eachValue(body, func(number protowire.Number, value []byte, _ uint64) error {
		if value == nil {
			return nil
		}
		return visit(number, value)
	})
}

// eachValue calls visit with every field of a message: bytes for length-delimited fields, the
// number for varints, and nothing for fixed-width fields, which no field used here has.
func eachValue(body []byte, visit func(protowire.Number, []byte, uint64) error) error {
	for len(body) > 0 {
		number, kind, n := protowire.ConsumeTag(body)
		if n < 0 {
			return protowire.ParseError(n)
		}
		body = body[n:]
		switch kind {
		case protowire.BytesType:
			value, n := protowire.ConsumeBytes(body)
			if n < 0 {
				return protowire.ParseError(n)
			}
			if value == nil {
				value = []byte{}
			}
			if err := visit(number, value, 0); err != nil {
				return err
			}
			body = body[n:]
		case protowire.VarintType:
			value, n := protowire.ConsumeVarint(body)
			if n < 0 {
				return protowire.ParseError(n)
			}
			if err := visit(number, nil, value); err != nil {
				return err
			}
			body = body[n:]
		default:
			n := protowire.ConsumeFieldValue(number, kind, body)
			if n < 0 {
				return protowire.ParseError(n)
			}
			body = body[n:]
		}
	}
	return nil
}
