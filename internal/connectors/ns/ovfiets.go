package ns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const maximumOVFietsPoints = 10

type ovfietsHours struct {
	// DayOfWeek is ISO, 1 for Monday through 7 for Sunday; checked against NS's own open state on
	// 2026-10-16.
	DayOfWeek     int    `json:"dayOfWeek"`
	StartTime     string `json:"startTime"`
	EndTime       string `json:"endTime"`
	ClosesNextDay bool   `json:"closesNextDay"`
}

type ovfietsPoint struct {
	Name         string         `json:"name"`
	StationCode  string         `json:"stationCode"`
	Description  string         `json:"description"`
	Open         string         `json:"open"`
	Street       string         `json:"street"`
	HouseNumber  string         `json:"houseNumber"`
	PostalCode   string         `json:"postalCode"`
	City         string         `json:"city"`
	OpeningHours []ovfietsHours `json:"openingHours"`
	Extra        struct {
		ServiceType string `json:"serviceType"`
		// RentalBikes is a count NS sends as a string.
		RentalBikes json.RawMessage `json:"rentalBikes"`
	} `json:"extra"`
	NearbyMeLocationID struct {
		Value string `json:"value"`
	} `json:"nearbyMeLocationId"`
}

type ovfietsResponse struct {
	Payload []struct {
		Locations []ovfietsPoint `json:"locations"`
	} `json:"payload"`
}

type ovfietsRequest struct {
	Station string `json:"station"`
}

func (s *Service) ovfiets(ctx context.Context, principalID string, request ovfietsRequest) tools.Result {
	list, failure := s.stationList(ctx, principalID)
	if failure != nil {
		return *failure
	}
	found, failure := pickStation(list, "station", request.Station)
	if failure != nil {
		return *failure
	}
	key := strings.Join([]string{toolOVFiets, found.code}, "|")
	if text, ok := s.cached(key); ok {
		return tools.Result{Text: text.(string)}
	}
	var response ovfietsResponse
	if err := s.places.get(ctx, "v2/ovfiets", url.Values{"station_code": {found.code}}, &response); err != nil {
		if errors.Is(err, errUnauthorized) {
			err = errNoOVFiets
		}
		return s.failure(ctx, principalID, toolOVFiets, err)
	}
	text := renderOVFiets(s.now().In(s.location), found, response)
	s.store(key, text)
	return tools.Result{Text: text}
}

func renderOVFiets(now time.Time, at station, response ovfietsResponse) string {
	var points []ovfietsPoint
	for _, item := range response.Payload {
		for _, point := range item.Locations {
			if point.StationCode == "" || strings.EqualFold(point.StationCode, at.code) {
				points = append(points, point)
			}
		}
	}
	if len(points) == 0 {
		return fmt.Sprintf("NS lists no OV-fiets rental point at %s.", at.name)
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "OV-fiets rental points at %s, bikes available now as of %s (Dutch local time):\n", at.name, now.Format("Mon 2 Jan 15:04"))
	for index, point := range points {
		if index == maximumOVFietsPoints {
			break
		}
		name := text(point.Description)
		if name == "" {
			name = text(point.Name)
		}
		if kind := ovfietsKind(point); kind != "" {
			name += " (" + kind + ")"
		}
		address := joinPresent(", ",
			joinPresent(" ", text(point.Street), text(point.HouseNumber)),
			joinPresent(" ", text(point.PostalCode), text(point.City)),
		)
		fmt.Fprintf(&builder, "- %s: %s", name, joinPresent("; ", bikesText(point.Extra.RentalBikes), openText(point.Open), hoursText(point.OpeningHours, now), address))
		builder.WriteString("\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// ovfietsKind trusts serviceType, which only staffed points carry as "Bemenst", before the location
// identifier, which starts with "Bemenst" or "Zelfservice".
func ovfietsKind(point ovfietsPoint) string {
	identifier := strings.TrimSpace(point.NearbyMeLocationID.Value)
	switch {
	case strings.EqualFold(strings.TrimSpace(point.Extra.ServiceType), "Bemenst"):
		return "staffed"
	case strings.HasPrefix(identifier, "Zelfservice"):
		return "self-service"
	case strings.HasPrefix(identifier, "Bemenst"):
		return "staffed"
	default:
		return ""
	}
}

func bikesText(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		value = string(raw)
	}
	count, err := strconv.Atoi(strings.TrimSpace(value))
	switch {
	case err != nil || count < 0:
		return "count not reported"
	case count == 0:
		return "none available"
	case count == 1:
		return "1 bike available"
	default:
		return fmt.Sprintf("%d bikes available", count)
	}
}

func openText(state string) string {
	switch strings.TrimSpace(state) {
	case "Yes":
		return "open now"
	case "No":
		return "closed now"
	default:
		return ""
	}
}

func hoursText(hours []ovfietsHours, now time.Time) string {
	if len(hours) == 0 {
		return "opening hours not reported"
	}
	today := int(now.Weekday())
	if today == 0 {
		today = 7
	}
	for _, entry := range hours {
		if entry.DayOfWeek != today {
			continue
		}
		start, end := clip(text(entry.StartTime), 5), clip(text(entry.EndTime), 5)
		switch {
		case start == "00:00" && end == "24:00":
			return "open all day today"
		case entry.ClosesNextDay && end != "24:00":
			return fmt.Sprintf("today %s–%s (next day)", start, end)
		default:
			return fmt.Sprintf("today %s–%s", start, end)
		}
	}
	return "closed all day today"
}
