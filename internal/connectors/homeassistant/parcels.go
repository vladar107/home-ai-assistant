package homeassistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/parcel"
)

// parcelReadTTL is how long one household-wide read, successful or not, serves every principal's
// Read, so a tick that refreshes each user's cards reads Home Assistant once.
const parcelReadTTL = 30 * time.Second

// Failures of Track, which the caller turns into a parcel followed by mail or a prompt for the
// postcode.
var (
	// ErrFollowedByMail means no integration can follow the number for this user: its carrier has
	// no number-tracking integration, the carrier is ambiguous, or the user has no assigned entry
	// that can take it.
	ErrFollowedByMail = parcel.ErrFollowedByMail
	// ErrPostcodeMissing means the carrier's integration needs the user's postcode and they have none.
	ErrPostcodeMissing = parcel.ErrPostcodeMissing
	// ErrInvalidNumber means the number is not a tracking number.
	ErrInvalidNumber = parcel.ErrInvalidNumber
)

type parcelShape int

const (
	// accountShape is one sensor per account whose attributes list enroute and delivered packages.
	accountShape parcelShape = iota + 1
	// normalisedShape is the family of carrier integrations that publish a parcels list on several
	// summary sensors.
	normalisedShape
)

// parcelDomains is the closed list of Home Assistant integrations whose sensors are read, with the
// carrier each one's parcels belong to.
var parcelDomains = map[string]struct {
	shape   parcelShape
	carrier string
}{
	"postnl": {accountShape, "postnl"},
	"dhl_nl": {normalisedShape, "dhl_nl"},
	"dhl":    {normalisedShape, "dhl"},
	"gls":    {normalisedShape, "gls"},
}

// numberTracking maps the domains whose services take a number to the field that picks the entry:
// the config entry for one, the postcode for the other.
var numberTracking = map[string]string{"dhl": "config_entry_id", "gls": "postal_code"}

const (
	serviceTrack   = "track_parcel"
	serviceUntrack = "untrack_parcel"
)

// sensorSuffixes are the unique ID endings of the summary sensors, with whether each is read. A
// unique ID matches its longest suffix, so an outgoing sensor is never taken for an incoming one.
var sensorSuffixes = []struct {
	suffix string
	read   bool
}{
	{"_incoming_parcels", true},
	{"_en_route_to_pickup_point", true},
	{"_awaiting_pickup", true},
	{"_delivered_parcels", true},
	{"_outgoing_parcels", false},
	{"_outgoing_delivered_parcels", false},
	{"_next_delivery", false},
	{"_last_update", false},
}

type parcelSensor struct {
	entityID string
	entryID  string
	domain   string
}

type discoveredEntry struct {
	domain string
	title  string
	loaded bool
}

type entrySnapshot struct {
	domain  string
	title   string
	state   parcel.SourceState
	readAt  time.Time
	parcels []parcel.Parcel
}

type refreshFlight struct {
	done chan struct{}
	err  error
}

// ParcelSource reads the parcels of the household's carrier integrations in Home Assistant and
// adds numbers to them. It is the Home Assistant side of Parcels: one household-wide read of the
// config entries assigned to someone, split by the principal each is assigned to, plus the numbers
// a principal added.
type ParcelSource struct {
	client   *client
	repo     parcel.Repository
	carriers parcel.Carriers
	logger   *slog.Logger
	now      func() time.Time
	assigned map[string][]string
	wanted   map[string]bool

	mu      sync.Mutex
	entries map[string]*entrySnapshot
	zone    *time.Location
	readAt  time.Time
	// read reports that a refresh was tried, and loaded that one reached Home Assistant.
	read    bool
	loaded  bool
	lastErr error
	flight  *refreshFlight

	watchMu sync.Mutex
	// names maps a lowercased friendly name to the parcel entity that carries it.
	names   map[string]string
	exposed map[string]bool
	warned  map[string]bool
}

var _ parcel.Sources = (*ParcelSource)(nil)

// NewParcelSource returns the parcel source over the connector's Home Assistant connection.
// assignments maps a principal to the config entry IDs that are theirs; an entry assigned to two
// principals is refused. Only assigned entries are ever read. The connector tells the source what
// the live context lists, to warn when a parcel entity is exposed to Assist; call it before the
// service handles turns.
func (s *Service) NewParcelSource(assignments map[string][]string, repo parcel.Repository) (*ParcelSource, error) {
	if repo == nil {
		return nil, errors.New("the parcel source needs a parcel repository")
	}
	assigned := make(map[string][]string, len(assignments))
	wanted := map[string]bool{}
	owners := map[string]string{}
	for principalID, ids := range assignments {
		assigned[principalID] = slices.Clone(ids)
		for _, id := range ids {
			if other, taken := owners[id]; taken && other != principalID {
				return nil, errors.New("a Home Assistant config entry is assigned to two principals")
			}
			owners[id] = principalID
			wanted[id] = true
		}
	}
	source := &ParcelSource{
		client: s.client, repo: repo, carriers: parcel.DefaultCarriers(), logger: s.logger, now: s.now, assigned: assigned, wanted: wanted,
		entries: map[string]*entrySnapshot{}, zone: time.UTC, names: map[string]string{}, exposed: map[string]bool{}, warned: map[string]bool{},
	}
	s.parcels = source
	return source, nil
}

func (p *ParcelSource) Assigned(principalID string) []string {
	return slices.Clone(p.assigned[principalID])
}

// refresh runs one read at a time: callers that arrive during one wait for its result, and a read
// within the TTL, failed ones included, is not repeated. The read runs on its own goroutine and
// outlives a caller that gives up, so a caller that is cancelled, as at shutdown, returns at once;
// the read touches only Home Assistant and the source's memory, never the store.
func (p *ParcelSource) refresh(ctx context.Context) error {
	p.mu.Lock()
	if p.read && p.now().Sub(p.readAt) < parcelReadTTL {
		err := p.lastErr
		p.mu.Unlock()
		return err
	}
	flight := p.flight
	if flight == nil {
		flight = &refreshFlight{done: make(chan struct{})}
		p.flight = flight
		go p.fly(context.WithoutCancel(ctx), flight)
	}
	p.mu.Unlock()
	select {
	case <-flight.done:
		return flight.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *ParcelSource) fly(ctx context.Context, flight *refreshFlight) {
	readCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	err := p.runRefresh(readCtx)
	cancel()

	p.mu.Lock()
	p.flight, p.lastErr, p.read, p.readAt = nil, err, true, p.now()
	flight.err = err
	close(flight.done)
	p.mu.Unlock()
}

type entryResult struct {
	domain, title string
	state         parcel.SourceState
	parcels       []parcel.Parcel
	replace       bool
	readAt        time.Time
}

func (p *ParcelSource) runRefresh(ctx context.Context) error {
	entries, sensors, names, err := p.discover(ctx)
	if err != nil {
		p.mu.Lock()
		for _, snap := range p.entries {
			if snap.state == parcel.SourceOK {
				snap.state = parcel.SourceStale
			}
		}
		p.mu.Unlock()
		p.logger.WarnContext(ctx, "homeassistant.parcels.discovery_failed", "error", err)
		return err
	}
	zone := p.loadZone(ctx)
	now := p.now()
	byEntry := map[string][]parcelSensor{}
	for _, sensor := range sensors {
		byEntry[sensor.entryID] = append(byEntry[sensor.entryID], sensor)
	}
	ids := make([]string, 0, len(p.wanted))
	for id := range p.wanted {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	results := map[string]entryResult{}
	friendly := map[string]string{}
	unreachable := false
	for _, id := range ids {
		found, listed := entries[id]
		if !listed {
			results[id] = entryResult{state: parcel.SourceMissing, replace: true}
			continue
		}
		result := entryResult{domain: found.domain, title: found.title, state: parcel.SourceStale}
		switch {
		case !found.loaded || unreachable:
		case len(byEntry[id]) == 0:
			p.logger.WarnContext(ctx, "homeassistant.parcels.no_sensors", "domain", found.domain)
		default:
			parcels, entryNames, dropped, err := p.readEntry(ctx, id, found.domain, byEntry[id], zone)
			if err != nil {
				p.logger.WarnContext(ctx, "homeassistant.parcels.read_failed", "domain", found.domain, "error", err)
				var failed *apiError
				unreachable = errors.As(err, &failed) && (failed.class == errUnavailable.class || failed.class == errTimeout.class || failed.class == errUnauthorized.class)
				break
			}
			if dropped > 0 {
				p.logger.WarnContext(ctx, "homeassistant.parcels.items_dropped", "domain", found.domain, "count", dropped)
			}
			result.state, result.parcels, result.replace, result.readAt = parcel.SourceOK, parcels, true, now
			for name, entityID := range entryNames {
				friendly[name] = entityID
			}
		}
		results[id] = result
	}

	p.mu.Lock()
	p.loaded, p.zone = true, zone
	for id, result := range results {
		snap := p.entries[id]
		if snap == nil {
			snap = &entrySnapshot{}
			p.entries[id] = snap
		}
		if result.domain != "" {
			snap.domain, snap.title = result.domain, result.title
		}
		snap.state = result.state
		if result.replace {
			snap.parcels = result.parcels
		}
		if !result.readAt.IsZero() {
			snap.readAt = result.readAt
		}
	}
	p.mu.Unlock()

	p.watchMu.Lock()
	for name, entityID := range names {
		p.names[name] = entityID
	}
	for name, entityID := range friendly {
		p.names[name] = entityID
	}
	p.watchMu.Unlock()
	p.checkExposure(ctx)
	return nil
}

// loadZone reads Home Assistant's own time zone, which timestamps without an offset are in; a
// failed read keeps the zone already known.
func (p *ParcelSource) loadZone(ctx context.Context) *time.Location {
	p.mu.Lock()
	zone := p.zone
	p.mu.Unlock()
	var config struct {
		TimeZone string `json:"time_zone"`
	}
	if err := p.client.getJSON(ctx, "/api/config", &config); err == nil && config.TimeZone != "" {
		if loaded, err := time.LoadLocation(config.TimeZone); err == nil {
			return loaded
		}
	}
	return zone
}

type registryEntity struct {
	EntityID      string  `json:"entity_id"`
	Platform      string  `json:"platform"`
	UniqueID      string  `json:"unique_id"`
	ConfigEntryID string  `json:"config_entry_id"`
	DisabledBy    *string `json:"disabled_by"`
	Name          string  `json:"name"`
	OriginalName  string  `json:"original_name"`
}

type configEntry struct {
	EntryID string `json:"entry_id"`
	Domain  string `json:"domain"`
	Title   string `json:"title"`
	State   string `json:"state"`
}

// discover finds the config entries of the listed domains and the parcel sensors of the assigned
// ones over the WebSocket API, on one connection. It also returns the names of every entity of a
// listed domain's entries, for the exposure check.
func (p *ParcelSource) discover(ctx context.Context) (map[string]discoveredEntry, []parcelSensor, map[string]string, error) {
	domains := make([]string, 0, len(parcelDomains))
	for domain := range parcelDomains {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	commands := []map[string]any{{"type": "config/entity_registry/list"}}
	for _, domain := range domains {
		commands = append(commands, map[string]any{"type": "config_entries/get", "domain": domain})
	}
	results, err := p.client.websocketCommands(ctx, commands...)
	if err != nil {
		return nil, nil, nil, err
	}
	entries := map[string]discoveredEntry{}
	for index, domain := range domains {
		var listed []configEntry
		if err := json.Unmarshal(results[index+1], &listed); err != nil {
			return nil, nil, nil, errMalformed
		}
		for _, entry := range listed {
			if entry.Domain == domain && entry.EntryID != "" {
				entries[entry.EntryID] = discoveredEntry{domain: domain, title: entry.Title, loaded: entry.State == "loaded"}
			}
		}
	}
	var registry []registryEntity
	if err := json.Unmarshal(results[0], &registry); err != nil {
		return nil, nil, nil, errMalformed
	}
	var sensors []parcelSensor
	names := map[string]string{}
	for _, entity := range registry {
		entry, known := entries[entity.ConfigEntryID]
		if !known || entity.Platform != entry.domain || !entityIDPattern.MatchString(entity.EntityID) {
			continue
		}
		for _, name := range []string{entity.Name, entity.OriginalName} {
			if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
				names[name] = entity.EntityID
			}
		}
		if p.wanted[entity.ConfigEntryID] && entity.DisabledBy == nil && strings.HasPrefix(entity.EntityID, "sensor.") && readable(entry.domain, entity.UniqueID) {
			sensors = append(sensors, parcelSensor{entityID: entity.EntityID, entryID: entity.ConfigEntryID, domain: entry.domain})
		}
	}
	sort.Slice(sensors, func(i, j int) bool { return sensors[i].entityID < sensors[j].entityID })
	return entries, sensors, names, nil
}

// readable reports whether a sensor with the unique ID holds incoming parcels of the domain.
func readable(domain, uniqueID string) bool {
	switch parcelDomains[domain].shape {
	case accountShape:
		return strings.HasSuffix(uniqueID, "_delivery")
	case normalisedShape:
		best, read := 0, false
		for _, candidate := range sensorSuffixes {
			if strings.HasSuffix(uniqueID, candidate.suffix) && len(candidate.suffix) > best {
				best, read = len(candidate.suffix), candidate.read
			}
		}
		return read
	}
	return false
}

// accountPackage is the part of an account sensor's package the source reads; the status message
// and the link are outside text and are never decoded.
type accountPackage struct {
	Key              string `json:"key"`
	Name             string `json:"name"`
	Delivered        bool   `json:"delivered"`
	DeliveryDate     string `json:"delivery_date"`
	PlannedDate      string `json:"planned_date"`
	PlannedFrom      string `json:"planned_from"`
	PlannedTo        string `json:"planned_to"`
	ExpectedDatetime string `json:"expected_datetime"`
	ExpectedFrom     string `json:"expected_from"`
	ExpectedTo       string `json:"expected_to"`
}

// normalisedParcel is the part of a normalised parcel the source reads. Its raw and history
// attributes carry addresses and other people's names, so they are not fields here.
type normalisedParcel struct {
	Barcode     string `json:"barcode"`
	Sender      string `json:"sender"`
	Status      string `json:"status"`
	PlannedFrom string `json:"planned_from"`
	PlannedTo   string `json:"planned_to"`
	DeliveredAt string `json:"delivered_at"`
	PickupPoint string `json:"pickup_point"`
}

type parcelState struct {
	State      string `json:"state"`
	Attributes struct {
		FriendlyName string             `json:"friendly_name"`
		Enroute      []accountPackage   `json:"enroute"`
		Delivered    []accountPackage   `json:"delivered"`
		Parcels      []normalisedParcel `json:"parcels"`
	} `json:"attributes"`
}

// readEntry reads the entry's sensors and returns its parcels, the friendly names of its entities,
// and how many items it dropped as unusable. Any failed sensor fails the entry, so a partial read
// never replaces the last parcels.
func (p *ParcelSource) readEntry(ctx context.Context, entryID, domain string, sensors []parcelSensor, zone *time.Location) ([]parcel.Parcel, map[string]string, int, error) {
	spec := parcelDomains[domain]
	names := map[string]string{}
	byKey := map[string]parcel.Parcel{}
	dropped := 0
	for _, sensor := range sensors {
		var state parcelState
		if err := p.client.getJSON(ctx, "/api/states/"+sensor.entityID, &state); err != nil {
			return nil, nil, 0, err
		}
		if state.State == "unavailable" || state.State == "unknown" {
			return nil, nil, 0, errUnavailable
		}
		if name := strings.ToLower(strings.TrimSpace(state.Attributes.FriendlyName)); name != "" {
			names[name] = sensor.entityID
		}
		var found []parcel.Parcel
		switch spec.shape {
		case accountShape:
			for _, item := range state.Attributes.Enroute {
				found = append(found, p.fromAccount(spec.carrier, entryID, item, false, zone))
			}
			for _, item := range state.Attributes.Delivered {
				found = append(found, p.fromAccount(spec.carrier, entryID, item, true, zone))
			}
		case normalisedShape:
			for _, item := range state.Attributes.Parcels {
				found = append(found, p.fromNormalised(spec.carrier, entryID, item, zone))
			}
		}
		for _, item := range found {
			if item.SourceKey == "" || item.Validate() != nil {
				dropped++
				continue
			}
			if stored, repeated := byKey[item.SourceKey]; !repeated || item.Status.Supersedes(stored.Status) {
				byKey[item.SourceKey] = item
			}
		}
	}
	parcels := make([]parcel.Parcel, 0, len(byKey))
	for _, item := range byKey {
		parcels = append(parcels, item)
	}
	sort.Slice(parcels, func(i, j int) bool { return parcels[i].SourceKey < parcels[j].SourceKey })
	return parcels, names, dropped, nil
}

// fromAccount maps an account integration's package. Only its delivered flag is a structured
// status: the status message is the carrier's free text, so a package that is not delivered is in
// transit. The shipment key is the barcode, the country, and the postcode joined by hyphens, so its
// first part is the tracking number, which links the parcel and matches it to its mail, when it
// has the shape of one of the carrier's numbers or of an international postal item's.
func (p *ParcelSource) fromAccount(carrier, entryID string, item accountPackage, delivered bool, zone *time.Location) parcel.Parcel {
	result := parcel.Parcel{
		Carrier: carrier, Title: parcel.PlainText(item.Name, 200), Source: parcel.SourceHomeAssistant,
		SourceKey: sourceKey(entryID, item.Key), Status: parcel.StatusInTransit,
	}
	if known, found := p.carriers.ByID(carrier); found {
		barcode, _, _ := strings.Cut(strings.TrimSpace(item.Key), "-")
		if number, ok := parcel.NormalizeNumber(barcode); ok && (postalItemPattern.MatchString(number) ||
			slices.ContainsFunc(p.carriers.Detect(number), func(match parcel.Carrier) bool { return match.ID == carrier })) {
			result.Number, result.Link = number, known.Link(number)
		}
	}
	if delivered || item.Delivered {
		result.Status = parcel.StatusDelivered
		result.DeliveredAt = parseMoment(item.DeliveryDate, zone)
		return result
	}
	switch {
	case item.ExpectedFrom != "":
		result.WindowFrom, result.WindowTo = parseMoment(item.ExpectedFrom, zone), parseMoment(item.ExpectedTo, zone)
	case item.PlannedFrom != "":
		result.WindowFrom, result.WindowTo = parseMoment(item.PlannedFrom, zone), parseMoment(item.PlannedTo, zone)
	case item.ExpectedDatetime != "":
		result.WindowFrom = parseMoment(item.ExpectedDatetime, zone)
	default:
		result.WindowFrom = parseMoment(item.PlannedDate, zone)
	}
	return result
}

func (p *ParcelSource) fromNormalised(carrier, entryID string, item normalisedParcel, zone *time.Location) parcel.Parcel {
	result := parcel.Parcel{
		Carrier: carrier, Sender: parcel.PlainText(item.Sender, 100), Source: parcel.SourceHomeAssistant,
		SourceKey: sourceKey(entryID, item.Barcode), Status: mapStatus(item.Status),
		WindowFrom: parseMoment(item.PlannedFrom, zone), WindowTo: parseMoment(item.PlannedTo, zone),
		DeliveredAt: parseMoment(item.DeliveredAt, zone), PickupPoint: parcel.PlainText(item.PickupPoint, 200),
	}
	if number, ok := parcel.NormalizeNumber(item.Barcode); ok {
		result.Number = number
		if known, found := p.carriers.ByID(carrier); found {
			result.Link = known.Link(number)
		}
	}
	return result
}

// mapStatus reads a normalised integration's status; the family's "returning" is a parcel going
// back to its sender, which the user needs to hear about as a problem.
func mapStatus(value string) parcel.Status {
	if strings.EqualFold(strings.TrimSpace(value), "returning") {
		return parcel.StatusProblem
	}
	return parcel.ParseStatus(value)
}

// sourceKey is a parcel's SourceKey: "<config entry ID>:<package key or barcode>". Entry IDs hold
// no colon, so the first colon splits the two.
func sourceKey(entryID, key string) string {
	if key = strings.TrimSpace(key); key == "" {
		return ""
	}
	return entryID + ":" + key
}

var (
	offsetLayouts = []string{time.RFC3339Nano}
	naiveLayouts  = []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02"}
)

// parseMoment reads a timestamp; one without an offset is in Home Assistant's time zone.
func parseMoment(value string, zone *time.Location) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range offsetLayouts {
		if moment, err := time.Parse(layout, value); err == nil {
			return moment.UTC()
		}
	}
	for _, layout := range naiveLayouts {
		if moment, err := time.ParseInLocation(layout, value, zone); err == nil {
			return moment.UTC()
		}
	}
	return time.Time{}
}

// List returns the principal's assigned sources with their state, from the last read: it never
// reaches Home Assistant, since the settings page renders it on the update loop. Before the first
// read every source is unknown.
func (p *ParcelSource) List(_ context.Context, principalID string) ([]parcel.SourceInfo, error) {
	assigned := p.assigned[principalID]
	if len(assigned) == 0 {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	unread := parcel.SourceMissing
	switch {
	case !p.read:
		unread = parcel.SourceUnknown
	case !p.loaded:
		unread = parcel.SourceStale
	}
	infos := make([]parcel.SourceInfo, 0, len(assigned))
	for _, id := range assigned {
		info := parcel.SourceInfo{EntryID: id, State: unread}
		if snap := p.entries[id]; snap != nil {
			info.Domain, info.Title, info.State, info.ReadAt = snap.domain, snap.title, snap.state, snap.readAt
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func (p *ParcelSource) hasLoaded() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loaded
}

// Read returns the principal's parcels: those of the entries assigned to them, whose numbers
// nobody else added, and those of any assigned entry whose number they added. A principal with no
// assigned entry has none, since a number is only ever added to the adder's own entry, so their
// reading never waits on Home Assistant or fails with it.
func (p *ParcelSource) Read(ctx context.Context, principalID string) (parcel.Reading, error) {
	assigned := p.assigned[principalID]
	if len(assigned) == 0 {
		return parcel.Reading{}, nil
	}
	if err := p.refresh(ctx); err != nil && !p.hasLoaded() {
		return parcel.Reading{}, err
	}
	type held struct {
		entryID string
		state   parcel.SourceState
		parcels []parcel.Parcel
	}
	p.mu.Lock()
	var snapshots []held
	var reading parcel.Reading
	for _, id := range assigned {
		snap := p.entries[id]
		if snap != nil && snap.state == parcel.SourceStale {
			reading.Stale = true
		}
		if snap == nil || snap.state != parcel.SourceMissing && snap.readAt.IsZero() {
			reading.Unread = true
		}
	}
	for id, snap := range p.entries {
		snapshots = append(snapshots, held{id, snap.state, slices.Clone(snap.parcels)})
	}
	p.mu.Unlock()
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].entryID < snapshots[j].entryID })

	owners := map[string][]string{}
	placed := map[string]int{}
	for _, snap := range snapshots {
		mine := slices.Contains(assigned, snap.entryID)
		for _, item := range snap.parcels {
			include := mine
			key := ""
			if item.Number != "" {
				key = item.Carrier + "|" + item.Number
				listed, known := owners[key]
				if !known {
					var err error
					if listed, err = p.repo.AddedNumberOwners(ctx, item.Carrier, item.Number); err != nil {
						return parcel.Reading{}, fmt.Errorf("read the parcel owners: %w", err)
					}
					owners[key] = listed
				}
				if len(listed) > 0 {
					include = slices.Contains(listed, principalID)
				}
			}
			if !include {
				continue
			}
			item.PrincipalID = principalID
			if at, repeated := placed[key]; key != "" && repeated {
				if item.Status.Supersedes(reading.Parcels[at].Status) {
					reading.Parcels[at] = item
				}
				continue
			}
			if key != "" {
				placed[key] = len(reading.Parcels)
			}
			reading.Parcels = append(reading.Parcels, item)
			if snap.state == parcel.SourceStale {
				reading.Stale = true
			}
		}
	}
	return reading, nil
}

// accountLiveness is how long after its last good read an account source still counts as listing
// every parcel of its carrier, so a short outage does not let mail duplicate its parcels while a
// broken integration leaves them to mail.
const accountLiveness = 24 * time.Hour

// AccountCarriers maps the carrier of each account-shape entry assigned to the principal that read
// within accountLiveness to whether every parcel it lists has its tracking number.
func (p *ParcelSource) AccountCarriers(principalID string) map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	carriers := map[string]bool{}
	for _, id := range p.assigned[principalID] {
		snap := p.entries[id]
		if snap == nil || parcelDomains[snap.domain].shape != accountShape || snap.readAt.IsZero() || now.Sub(snap.readAt) > accountLiveness ||
			(snap.state != parcel.SourceOK && snap.state != parcel.SourceStale) {
			continue
		}
		carrier := parcelDomains[snap.domain].carrier
		numbered, seen := carriers[carrier]
		carriers[carrier] = (numbered || !seen) && !slices.ContainsFunc(snap.parcels, func(item parcel.Parcel) bool { return item.Number == "" })
	}
	return carriers
}

// checkExposure logs one warning for each parcel entity whose name the live context lists, since
// Assist would then show every user's parcels to everyone.
func (p *ParcelSource) checkExposure(ctx context.Context) {
	p.watchMu.Lock()
	var found []string
	for name, entityID := range p.names {
		if p.exposed[name] && !p.warned[entityID] {
			p.warned[entityID] = true
			found = append(found, entityID)
		}
	}
	p.watchMu.Unlock()
	sort.Strings(found)
	for _, entityID := range found {
		p.logger.WarnContext(ctx, "homeassistant.parcels.exposed_to_assist", "entity_id", entityID)
	}
}

// noteExposed records the names the live context lists and warns about parcel entities among them.
func (p *ParcelSource) noteExposed(ctx context.Context, exposed []exposedEntity) {
	names := map[string]bool{}
	for _, entity := range exposed {
		for _, name := range entity.names {
			names[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	p.watchMu.Lock()
	p.exposed = names
	p.watchMu.Unlock()
	p.checkExposure(ctx)
}

var (
	// postalItemPattern is the UPU S10 shape of an international postal item, such as one sent
	// from abroad, which the carrier's own patterns do not cover.
	postalItemPattern = regexp.MustCompile(`^[A-Z]{2}[0-9]{9}[A-Z]{2}$`)
	postcodePattern   = regexp.MustCompile(`^[A-Z0-9]{3,12}$`)
	// titlePostcode reads the postcode a GLS hub is titled with, "GLS (<postcode>)".
	titlePostcode = regexp.MustCompile(`\(([^()]+)\)\s*$`)
)

func normalisePostcode(value string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(value))
}

// callTrackingService is the only place the source writes to Home Assistant: the add and remove
// services of the number-tracking domains.
func (p *ParcelSource) callTrackingService(ctx context.Context, domain, service string, body map[string]any) error {
	if _, tracks := numberTracking[domain]; !tracks || (service != serviceTrack && service != serviceUntrack) {
		return errBadRequest
	}
	return p.client.postJSON(ctx, "/api/services/"+domain+"/"+service, body, nil)
}

// resolveCarrier names the carrier whose integration tracks the number. Without a carrier the
// number must point at exactly one.
func (p *ParcelSource) resolveCarrier(number, carrier string) (string, error) {
	carrier = strings.ToLower(strings.TrimSpace(carrier))
	if carrier == "" {
		detected := p.carriers.Detect(number)
		if len(detected) != 1 {
			return "", ErrFollowedByMail
		}
		carrier = detected[0].ID
	}
	if _, tracks := numberTracking[carrier]; !tracks {
		return "", ErrFollowedByMail
	}
	return carrier, nil
}

func (p *ParcelSource) hasEntry(principalID, domain string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.ContainsFunc(p.assigned[principalID], func(id string) bool {
		snap := p.entries[id]
		return snap != nil && snap.domain == domain && snap.state != parcel.SourceMissing
	})
}

// trackingEntry picks the entry of the domain the number is added to: the user's assigned dhl
// entry, or the assigned gls hub whose postcode is the user's. It returns the postcode to send,
// and whether the entry already lists the number.
func (p *ParcelSource) trackingEntry(ctx context.Context, principalID, domain, number string) (string, string, bool, error) {
	if err := p.refresh(ctx); err != nil && !p.hasLoaded() {
		return "", "", false, err
	}
	if !p.hasEntry(principalID, domain) {
		return "", "", false, ErrFollowedByMail
	}
	postcode := ""
	if numberTracking[domain] == "postal_code" {
		settings, err := p.repo.LoadSettings(ctx, principalID)
		if err != nil {
			return "", "", false, fmt.Errorf("read the parcel settings: %w", err)
		}
		if postcode = normalisePostcode(settings.Postcode); !postcodePattern.MatchString(postcode) {
			return "", "", false, ErrPostcodeMissing
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range p.assigned[principalID] {
		snap := p.entries[id]
		if snap == nil || snap.domain != domain || snap.state == parcel.SourceMissing {
			continue
		}
		if postcode != "" {
			if match := titlePostcode.FindStringSubmatch(snap.title); match == nil || normalisePostcode(match[1]) != postcode {
				continue
			}
		}
		listed := slices.ContainsFunc(snap.parcels, func(item parcel.Parcel) bool { return item.Number == number })
		return id, postcode, listed, nil
	}
	return "", "", false, ErrFollowedByMail
}

// Track adds the number to the user's own entry of the carrier's integration, with the postcode
// from their settings where the integration takes one, and records it as theirs. It reports false,
// with nothing written or recorded, when the entry already lists the number. The record of the
// proposal's outcome is the caller's.
func (p *ParcelSource) Track(ctx context.Context, principalID, number, carrier string) (parcel.AddedNumber, bool, error) {
	normalised, ok := parcel.NormalizeNumber(number)
	if !ok {
		return parcel.AddedNumber{}, false, ErrInvalidNumber
	}
	carrier, err := p.resolveCarrier(normalised, carrier)
	if err != nil {
		return parcel.AddedNumber{}, false, err
	}
	entryID, postcode, listed, err := p.trackingEntry(ctx, principalID, carrier, normalised)
	if err != nil {
		return parcel.AddedNumber{}, false, err
	}
	added := parcel.AddedNumber{PrincipalID: principalID, Carrier: carrier, Number: normalised, Domain: carrier, EntryID: entryID, AddedAt: p.now()}
	if listed {
		return added, false, nil
	}
	body := map[string]any{"tracking_code": normalised}
	if numberTracking[carrier] == "postal_code" {
		body["postal_code"] = postcode
	} else {
		body["config_entry_id"] = entryID
	}
	if err := p.callTrackingService(ctx, carrier, serviceTrack, body); err != nil {
		return parcel.AddedNumber{}, false, err
	}
	if err := p.repo.SaveAddedNumber(ctx, added); err != nil {
		if undoErr := p.callTrackingService(context.WithoutCancel(ctx), carrier, serviceUntrack, untrackBody(carrier, normalised, entryID)); undoErr != nil {
			p.logger.WarnContext(ctx, "homeassistant.parcels.compensation_failed", "principal_id", principalID, "carrier", carrier, "error", undoErr)
		}
		return parcel.AddedNumber{}, false, fmt.Errorf("record the added number: %w", err)
	}
	p.mu.Lock()
	p.read = false
	p.mu.Unlock()
	p.logger.InfoContext(ctx, "homeassistant.parcels.tracked", "principal_id", principalID, "carrier", carrier)
	return added, true, nil
}

// untrackBody is the remove service's data. The gls service has no selector and removes the number
// from whichever hub tracks it; the dhl one acts on one entry.
func untrackBody(domain, number, entryID string) map[string]any {
	body := map[string]any{"tracking_code": number}
	if numberTracking[domain] == "config_entry_id" {
		body["config_entry_id"] = entryID
	}
	return body
}

// Untrack removes a number the principal added from Home Assistant and deletes the record. When
// the removal would also take the number from someone else's parcels, it deletes only the record:
// another user added the same number, or, for gls, whose remove service acts on every hub, another
// entry lists it.
func (p *ParcelSource) Untrack(ctx context.Context, principalID, carrier, number string) error {
	normalised, ok := parcel.NormalizeNumber(number)
	if !ok {
		return ErrInvalidNumber
	}
	added, found, err := p.repo.LoadAddedNumber(ctx, principalID, carrier, normalised)
	if err != nil {
		return fmt.Errorf("read the added number: %w", err)
	}
	if !found {
		return nil
	}
	shared, err := p.sharedNumber(ctx, added)
	if err != nil {
		return err
	}
	if !shared {
		if err := p.callTrackingService(ctx, added.Domain, serviceUntrack, untrackBody(added.Domain, added.Number, added.EntryID)); err != nil {
			return err
		}
	}
	if err := p.repo.DeleteAddedNumber(ctx, principalID, carrier, normalised); err != nil {
		return fmt.Errorf("delete the added number: %w", err)
	}
	p.mu.Lock()
	p.read = false
	p.mu.Unlock()
	return nil
}

func (p *ParcelSource) sharedNumber(ctx context.Context, added parcel.AddedNumber) (bool, error) {
	owners, err := p.repo.AddedNumberOwners(ctx, added.Carrier, added.Number)
	if err != nil {
		return false, fmt.Errorf("read the number's owners: %w", err)
	}
	global := numberTracking[added.Domain] == "postal_code"
	for _, owner := range owners {
		if owner == added.PrincipalID {
			continue
		}
		other, found, err := p.repo.LoadAddedNumber(ctx, owner, added.Carrier, added.Number)
		if err != nil {
			return false, fmt.Errorf("read the number's owners: %w", err)
		}
		if found && (global || other.EntryID == added.EntryID) {
			return true, nil
		}
	}
	if !global {
		return false, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, snap := range p.entries {
		if id != added.EntryID && snap.domain == added.Domain && slices.ContainsFunc(snap.parcels, func(item parcel.Parcel) bool { return item.Number == added.Number }) {
			return true, nil
		}
	}
	return false, nil
}
