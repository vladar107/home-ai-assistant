package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connectors/bus"
	"github.com/vladar107/home-ai-assistant/internal/connectors/flights"
	"github.com/vladar107/home-ai-assistant/internal/connectors/grafana"
	"github.com/vladar107/home-ai-assistant/internal/connectors/homeassistant"
	"github.com/vladar107/home-ai-assistant/internal/connectors/mcp"
	"github.com/vladar107/home-ai-assistant/internal/connectors/nas"
	"github.com/vladar107/home-ai-assistant/internal/connectors/ns"
	"github.com/vladar107/home-ai-assistant/internal/connectors/places"
	"github.com/vladar107/home-ai-assistant/internal/connectors/web"
	userlocation "github.com/vladar107/home-ai-assistant/internal/location"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	"github.com/vladar107/home-ai-assistant/internal/testutil/bussim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/flightssim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/grafanasim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/homesim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/mcpsim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/nassim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/nssim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/pagesim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/placessim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
	"github.com/vladar107/home-ai-assistant/internal/testutil/weathersim"
	"github.com/vladar107/home-ai-assistant/internal/wiring"
)

// The world sections that start a connector's loopback fake. A section holds the fake's own seed,
// documented in the fake's package; "places" stays the user's saved places.
const (
	SectionTrains       = "trains"
	SectionBuses        = "buses"
	SectionWeather      = "weather"
	SectionPages        = "pages"
	SectionPlacesSearch = "places_search"
	SectionFlights      = "flights"
	SectionMonitoring   = "monitoring"
	SectionNAS          = "nas"
	SectionHome         = "home"
	SectionMCP          = "mcp"

	fakeKey           = "eval-fake-key"
	maxMCPServers     = 12
	maxMCPServerTools = 16
	unreachableURL    = "http://127.0.0.1:1"
)

var mcpServerName = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

func init() {
	for name, validate := range map[string]func(json.RawMessage) error{
		SectionTrains:       nssim.Validate,
		SectionBuses:        bussim.Validate,
		SectionWeather:      weathersim.Validate,
		SectionPages:        pagesim.Validate,
		SectionPlacesSearch: placessim.Validate,
		SectionFlights:      flightssim.Validate,
		SectionMonitoring:   grafanasim.Validate,
		SectionNAS:          nassim.Validate,
		SectionHome:         homesim.Validate,
		SectionMCP:          validateMCP,
	} {
		DefaultRegistry.RegisterWorldSection(name, validate)
	}
}

func validateMCP(raw json.RawMessage) error {
	servers, err := mcpSeeds(raw)
	if err != nil {
		return err
	}
	if len(servers) > maxMCPServers {
		return fmt.Errorf("at most %d servers", maxMCPServers)
	}
	for name, seed := range servers {
		if !mcpServerName.MatchString(name) {
			return fmt.Errorf("%s: a server name is a lowercase letter then up to 15 lowercase letters or digits", name)
		}
		if err := mcpsim.Validate(seed); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		tools, err := mcpToolNames(seed)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if len(tools) == 0 || len(tools) > maxMCPServerTools {
			return fmt.Errorf("%s: want 1 to %d tools, got %d", name, maxMCPServerTools, len(tools))
		}
	}
	return nil
}

func mcpSeeds(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var servers map[string]json.RawMessage
	if err := decodeStrict(raw, &servers); err != nil {
		return nil, err
	}
	return servers, nil
}

func mcpToolNames(seed json.RawMessage) ([]string, error) {
	var parsed mcpsim.Seed
	if err := simkit.Decode(seed, &parsed); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(parsed.Tools))
	for _, tool := range parsed.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

// startFake starts the section's fake if the case has the section, and queues its shutdown with
// the world's.
func startFake[S any](w *World, name string, start func(json.RawMessage) (S, error), closer func(S)) (S, bool, error) {
	var zero S
	raw, ok := w.Case.World[name]
	if !ok {
		return zero, false, nil
	}
	fake, err := start(raw)
	if err != nil {
		return zero, false, fmt.Errorf("world.%s: %w", name, err)
	}
	w.closers = append(w.closers, func() { closer(fake) })
	return fake, true, nil
}

// seedConnectorFakes starts the fake of every connector section the case has and points the
// connector at it; a connector without a section stays unconfigured.
func (w *World) seedConnectorFakes(in *wiring.Inputs) error {
	now := w.Clock.Now
	if fake, ok, err := startFake(w, SectionTrains, nssim.Start, (*nssim.Server).Close); err != nil {
		return err
	} else if ok {
		in.NS = &ns.Config{APIKey: fakeKey, BaseURL: fake.BaseURL(), PlacesBaseURL: fake.PlacesBaseURL(), Now: now}
	}
	if fake, ok, err := startFake(w, SectionBuses, bussim.Start, (*bussim.Server).Close); err != nil {
		return err
	} else if ok {
		in.Bus = &bus.Config{OVapiURL: fake.OVapiURL(), GTFSURL: fake.GTFSURL(), TransitousURL: fake.TransitousURL(), Now: now}
	}
	if err := w.seedWeb(in); err != nil {
		return err
	}
	if fake, ok, err := startFake(w, SectionFlights, flightssim.Start, (*flightssim.Server).Close); err != nil {
		return err
	} else if ok {
		in.Flights = &flights.Config{
			APIKey: fakeKey, MonthlyUnits: 1000, QuotaResetDay: 1, BaseURL: fake.URL(), Now: now,
			Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		}
	}
	if fake, ok, err := startFake(w, SectionMonitoring, grafanasim.Start, (*grafanasim.Server).Close); err != nil {
		return err
	} else if ok {
		in.Grafana = &grafana.Config{Token: fake.Token(), BaseURL: fake.URL(), Principals: []string{PrincipalID}, Now: now}
	}
	if tree, ok, err := startFake(w, SectionNAS, nassim.Start, (*nassim.Tree).Close); err != nil {
		return err
	} else if ok {
		in.NAS = &nas.Config{Roots: tree.Roots(PrincipalID)}
	}
	startHome := func(raw json.RawMessage) (*homesim.Server, error) { return homesim.StartAt(raw, now) }
	if fake, ok, err := startFake(w, SectionHome, startHome, (*homesim.Server).Close); err != nil {
		return err
	} else if ok {
		in.HA = &homeassistant.Config{Token: fake.Token(), BaseURL: fake.URL(), Now: now}
	}
	if _, has := w.Case.World[SectionParcels]; has {
		w.defaultSwitches = append(w.defaultSwitches, parcel.Integration)
	}
	return w.seedMCP(in)
}

// seedWeb serves the web connector's weather and page fakes through one router, so its address
// guard stays whole and nothing but the seeded names resolves.
func (w *World) seedWeb(in *wiring.Inputs) error {
	router := simkit.NewRouter()
	cfg := web.Config{Resolver: router, Dial: router.DialContext, Now: w.Clock.Now}
	weather, hasWeather, err := startFake(w, SectionWeather, weathersim.Start, (*weathersim.Server).Close)
	if err != nil {
		return err
	}
	if hasWeather {
		cfg.GeocodingURL, cfg.ForecastURL = weather.Mount(router)
		// The household's deployment names a home, so the first seeded place stands in for it and the
		// model reads the same weather guidance it reads there.
		var seed struct {
			Places []struct {
				Name string `json:"name"`
			} `json:"places"`
		}
		if err := json.Unmarshal(w.Case.World[SectionWeather], &seed); err == nil && len(seed.Places) > 0 {
			cfg.WeatherHome = seed.Places[0].Name
		}
	}
	pages, hasPages, err := startFake(w, SectionPages, pagesim.Start, (*pagesim.Server).Close)
	if err != nil {
		return err
	}
	if hasPages {
		pages.Mount(router)
	}
	if hasWeather || hasPages {
		in.Web = &cfg
	}
	return nil
}

// seedMCP serves each seeded catalogue server and switches it on for the principal, as the
// operator's catalogue and the user's own choice would.
func (w *World) seedMCP(in *wiring.Inputs) error {
	raw, ok := w.Case.World[SectionMCP]
	if !ok {
		return nil
	}
	seeds, err := mcpSeeds(raw)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(seeds))
	for name := range seeds {
		names = append(names, name)
	}
	slices.Sort(names)
	cfg := mcp.Config{Now: w.Clock.Now}
	for _, name := range names {
		fake, err := mcpsim.Start(seeds[name])
		if err != nil {
			return fmt.Errorf("world.mcp.%s: %w", name, err)
		}
		w.closers = append(w.closers, fake.Close)
		toolNames, err := mcpToolNames(seeds[name])
		if err != nil {
			return fmt.Errorf("world.mcp.%s: %w", name, err)
		}
		server := config.MCPServer{
			Name: name, Title: strings.ToUpper(name[:1]) + name[1:], Description: "Simulated catalogue server " + name,
			URL: fake.URL(), Auth: config.MCPAuthNone,
		}
		for _, tool := range toolNames {
			server.Tools = append(server.Tools, config.MCPTool{Name: tool})
		}
		cfg.Servers = append(cfg.Servers, server)
		w.defaultSwitches = append(w.defaultSwitches, mcp.IntegrationName(name))
	}
	in.MCP = &cfg
	return nil
}

// seedPlaces configures shared locations and the places connector behind them. Saved places need
// the location service, and place search is served by the fake when the case seeds it; without it
// every lookup fails fast on an address nothing listens on.
func (w *World) seedPlaces(in *wiring.Inputs) error {
	_, hasSaved := w.Case.World[SectionPlaces]
	fake, hasSearch, err := startFake(w, SectionPlacesSearch, placessim.Start, (*placessim.Server).Close)
	if err != nil {
		return err
	}
	if !hasSaved && !hasSearch {
		return nil
	}
	cfg := places.Config{OverpassURL: unreachableURL, TransitousURL: unreachableURL, Now: w.Clock.Now}
	if hasSearch {
		cfg.OverpassURL, cfg.TransitousURL = fake.OverpassURL(), fake.TransitousURL()
	}
	in.Core.Location = &wiring.LocationConfig{
		Policy:        userlocation.Policy{PinTTL: time.Hour, LiveSilence: time.Hour, LiveGrace: 15 * time.Minute},
		PruneInterval: time.Hour,
		Places:        cfg,
	}
	return nil
}
