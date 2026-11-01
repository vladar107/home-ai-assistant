package location

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolPlaceList   = "place_list"
	toolPlaceSave   = "place_save"
	toolPlaceUpdate = "place_update"
	toolPlaceDelete = "place_delete"

	// placeActTimeout lets a change made at once run as long as one made after Confirm.
	placeActTimeout = 45 * time.Second
)

// Proposer is the core approval service as the location service sees it.
type Proposer interface {
	Act(context.Context, string, approval.Proposal) (string, error)
}

// SetPlaceTools turns on the place tools that save, change, and delete places, which need the
// approval service, and the check that keeps a place in use from being deleted, at wiring time
// before turns start. Without a proposer, place_list is the only place tool.
func (s *Service) SetPlaceTools(proposer Proposer, dependents PlaceDependents) {
	s.proposer, s.dependents = proposer, dependents
}

const placeGuidance = "Places: the user's saved places, each a name, a point, and a radius in metres that counts as being there. place_list lists them with their ids. " +
	"While the user shares their live location, the turn line names the saved place they are at, and arriving at or leaving a place can alert a place watch or run a job anchored to it."

func placeWriteGuidance() string {
	return "place_save saves the user's latest shared location, a pin, a venue, or their live location, as a named place, such as when the user says save this as Gym or this is Office; it never takes coordinates, so ask the user to share a location in this chat first when they have none. " +
		fmt.Sprintf("radius_m is optional, %d by default, between %d and %d. ", DefaultRadius, MinimumRadius, MaximumRadius) +
		"place_update renames a place or changes its radius, and place_delete deletes one; a place that a watch or an anchored job uses cannot be deleted until those are removed or changed, and the tool names them. " +
		"When the user asked for it in this conversation and the conversation has read no outside content, each is done at once and the user can undo it; tell the user it is done. After outside content it is only proposed with Confirm and Reject buttons, so never claim it is saved. " +
		"Arrivals and leavings are noticed only while the user shares their live location in this chat, and may be noticed late when the phone pauses its updates; say so when you set up something that waits for one. " +
		fmt.Sprintf("Each user may have %d places.", MaximumPlacesPerPrincipal)
}

var placeListDefinition = tools.Definition{
	Name:        toolPlaceList,
	Description: "List the user's saved places with their ids, radii, and whether the live location shows the user there.",
	Risk:        tools.RiskRead,
	InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
}

var placeWriteDefinitions = []tools.Definition{
	{
		Name:        toolPlaceSave,
		Description: "Save the user's latest shared location as a named place. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		Timeout:     placeActTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","maxLength":40,"description":"A short name, such as Gym or Office."},` +
			`"radius_m":{"type":"integer","minimum":50,"maximum":2000,"description":"How far from the point still counts as being there, in metres; 150 by default."}},` +
			`"required":["name"],"additionalProperties":false}`),
	},
	{
		Name:        toolPlaceUpdate,
		Description: "Rename a saved place or change its radius. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		Timeout:     placeActTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","description":"The place's current name."},` +
			`"new_name":{"type":"string","maxLength":40,"description":"A new name."},` +
			`"radius_m":{"type":"integer","minimum":50,"maximum":2000,"description":"A new radius in metres."}},` +
			`"required":["name"],"additionalProperties":false}`),
	},
	{
		Name:        toolPlaceDelete,
		Description: "Delete a saved place that no watch or anchored job uses. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		Timeout:     placeActTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","description":"The place's name."}},` +
			`"required":["name"],"additionalProperties":false}`),
	},
}

var (
	_ tools.Provider    = (*Service)(nil)
	_ approval.Executor = (*Service)(nil)
	_ approval.Undoer   = (*Service)(nil)
)

func (s *Service) Contribute(context.Context, string) (tools.Contribution, error) {
	contribution := tools.Contribution{Guidance: placeGuidance, Definitions: []tools.Definition{placeListDefinition}}
	if s.proposer != nil {
		contribution.WriteGuidance = placeWriteGuidance()
		contribution.Definitions = append(contribution.Definitions, placeWriteDefinitions...)
	}
	return contribution, nil
}

func (s *Service) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	switch call.Name {
	case toolPlaceList:
		return s.listPlaces(ctx, principalID)
	case toolPlaceSave:
		return s.proposeSave(ctx, principalID, call)
	case toolPlaceUpdate:
		return s.proposeUpdate(ctx, principalID, call)
	case toolPlaceDelete:
		return s.proposeDelete(ctx, principalID, call)
	default:
		return placeError("unknown place tool"), nil
	}
}

type placeSaveArguments struct {
	Name    string `json:"name"`
	RadiusM int    `json:"radius_m"`
}

type placeUpdateArguments struct {
	Name    string  `json:"name"`
	NewName *string `json:"new_name"`
	RadiusM int     `json:"radius_m"`
}

type placeNameArguments struct {
	Name string `json:"name"`
}

// confirmedPlace is a place to save as proposed. It names the shared position it was proposed from
// rather than copying its point, which approvals would keep for good: a pin or venue by when it was
// shared, a live share by its message, whose latest position is saved.
type confirmedPlace struct {
	Name          string    `json:"name"`
	RadiusM       int       `json:"radius_m"`
	SharedAt      time.Time `json:"shared_at"`
	LiveMessageID int64     `json:"live_message_id,omitempty"`
}

// confirmedPlaceUpdate is the place as it will be, with its last change as proposed, so a place
// changed before the press is not overwritten.
type confirmedPlaceUpdate struct {
	PlaceID   string    `json:"place_id"`
	UpdatedAt time.Time `json:"updated_at"`
	Name      string    `json:"name"`
	RadiusM   int       `json:"radius_m"`
}

type confirmedPlaceDelete struct {
	PlaceID string `json:"place_id"`
}

func (s *Service) listPlaces(ctx context.Context, principalID string) (tools.Result, error) {
	places, err := s.repository.ListPlaces(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	if len(places) == 0 {
		return tools.Result{Text: "The user has no saved places."}, nil
	}
	inside := ""
	if fix, found, err := s.Current(ctx, principalID); err == nil && found && fix.Live(s.now()) {
		inside = s.insidePlace(ctx, fix)
	}
	lines := make([]string, 0, len(places))
	for _, place := range places {
		line := "id " + place.ID + ": " + describePlace(place)
		if inside == place.Name {
			line += "; the user is there now"
		}
		lines = append(lines, line)
	}
	sharing, err := s.SharingLive(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	text := strings.Join(lines, "\n")
	if !sharing {
		text += "\n" + NotSharingNote
	}
	return tools.Result{Text: text}, nil
}

// NotSharingNote tells the user that arrivals and leavings wait for a live share.
const NotSharingNote = "Arriving and leaving are noticed only while you share your live location in this chat, and you are not sharing it now."

// describePlace is a place as the user reads it, with its point rounded to four decimals.
func describePlace(place Place) string {
	return fmt.Sprintf("%s (%s, %s; radius %d m)", place.Name, rounded(place.Latitude), rounded(place.Longitude), place.RadiusM)
}

func rounded(value float64) string { return strconv.FormatFloat(value, 'f', 4, 64) }

// named finds the principal's place by name; a refusal is text for the model.
func (s *Service) named(ctx context.Context, principalID, name string) (Place, string, error) {
	places, err := s.repository.ListPlaces(ctx, principalID)
	if err != nil {
		return Place{}, "", err
	}
	if place, found := placeByName(places, name); found {
		return place, "", nil
	}
	if len(places) == 0 {
		return Place{}, "The user has no saved places.", nil
	}
	names := make([]string, 0, len(places))
	for _, place := range places {
		names = append(names, place.Name)
	}
	return Place{}, fmt.Sprintf("No place is called %q. The user's places: %s.", strings.TrimSpace(name), strings.Join(names, ", ")), nil
}

func placeByName(places []Place, name string) (Place, bool) {
	key := NameKey(name)
	for _, place := range places {
		if NameKey(place.Name) == key {
			return place, true
		}
	}
	return Place{}, false
}

// PlaceNamed is the principal's place with the name, compared without case.
func (s *Service) PlaceNamed(ctx context.Context, principalID, name string) (Place, bool, error) {
	places, err := s.repository.ListPlaces(ctx, principalID)
	if err != nil {
		return Place{}, false, err
	}
	place, found := placeByName(places, name)
	return place, found, nil
}

// PlaceByID is the principal's place with the ID.
func (s *Service) PlaceByID(ctx context.Context, principalID, placeID string) (Place, bool, error) {
	return s.repository.LoadPlace(ctx, principalID, strings.TrimSpace(placeID))
}

func (s *Service) proposeSave(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return placeError("Places cannot be saved here."), nil
	}
	var arguments placeSaveArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return placeError(err.Error()), nil
	}
	name, err := normalizePlaceName(arguments.Name)
	if err != nil {
		return placeError(err.Error()), nil
	}
	radius := arguments.RadiusM
	if radius == 0 {
		radius = DefaultRadius
	}
	if err := checkRadius(radius); err != nil {
		return placeError(err.Error()), nil
	}
	fix, found, err := s.Current(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	if !found {
		return placeError("The user has shared no location, or it has expired. Ask them to share a pin, a venue, or their live location in this chat first, then save it."), nil
	}
	places, err := s.repository.ListPlaces(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	if _, taken := placeByName(places, name); taken {
		return placeError(fmt.Sprintf("A place called %q already exists; change it with place_update or choose another name.", name)), nil
	}
	if len(places) >= MaximumPlacesPerPrincipal {
		return placeError(tooManyPlacesText()), nil
	}
	confirmed := confirmedPlace{Name: name, RadiusM: radius, SharedAt: fix.UpdatedAt, LiveMessageID: fix.LiveMessageID}
	canonical, err := json.Marshal(confirmed)
	if err != nil {
		return tools.Result{}, err
	}
	summary := fmt.Sprintf("Save your latest shared %s as the place %s, radius %d m.", sourceName(fix), name, radius)
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolPlaceSave, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "save", TargetKind: "place", TargetName: name}})
}

func (s *Service) proposeUpdate(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return placeError("Places cannot be changed here."), nil
	}
	var arguments placeUpdateArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return placeError(err.Error()), nil
	}
	place, refusal, err := s.named(ctx, principalID, arguments.Name)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return placeError(refusal), nil
	}
	next := confirmedPlaceUpdate{PlaceID: place.ID, UpdatedAt: place.UpdatedAt, Name: place.Name, RadiusM: place.RadiusM}
	var changes []string
	if arguments.NewName != nil {
		name, err := normalizePlaceName(*arguments.NewName)
		if err != nil {
			return placeError(err.Error()), nil
		}
		if NameKey(name) != NameKey(place.Name) {
			if _, taken, err := s.PlaceNamed(ctx, principalID, name); err != nil {
				return tools.Result{}, err
			} else if taken {
				return placeError(fmt.Sprintf("A place called %q already exists.", name)), nil
			}
		}
		if name != place.Name {
			changes = append(changes, "name: "+place.Name+" → "+name)
		}
		next.Name = name
	}
	if arguments.RadiusM != 0 {
		if err := checkRadius(arguments.RadiusM); err != nil {
			return placeError(err.Error()), nil
		}
		if arguments.RadiusM != place.RadiusM {
			changes = append(changes, fmt.Sprintf("radius: %d m → %d m", place.RadiusM, arguments.RadiusM))
		}
		next.RadiusM = arguments.RadiusM
	}
	if len(changes) == 0 {
		return placeError("That changes nothing; the place already reads exactly like this."), nil
	}
	canonical, err := json.Marshal(next)
	if err != nil {
		return tools.Result{}, err
	}
	summary := "Change the place " + place.Name + ":\n" + strings.Join(changes, "\n")
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolPlaceUpdate, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "change", TargetKind: "place", TargetName: place.Name, NewName: next.Name}})
}

func (s *Service) proposeDelete(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return placeError("Places cannot be deleted here."), nil
	}
	var arguments placeNameArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return placeError(err.Error()), nil
	}
	place, refusal, err := s.named(ctx, principalID, arguments.Name)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return placeError(refusal), nil
	}
	if refusal, err := s.inUse(ctx, place); err != nil || refusal != "" {
		if err != nil {
			return tools.Result{}, err
		}
		return placeError(refusal), nil
	}
	canonical, err := json.Marshal(confirmedPlaceDelete{PlaceID: place.ID})
	if err != nil {
		return tools.Result{}, err
	}
	summary := fmt.Sprintf("Delete the place %s (radius %d m).", place.Name, place.RadiusM)
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolPlaceDelete, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "delete", TargetKind: "place", TargetName: place.Name}})
}

// inUse refuses to delete a place a watch or an anchored job uses, naming them.
func (s *Service) inUse(ctx context.Context, place Place) (string, error) {
	if s.dependents == nil {
		return "", nil
	}
	dependents, err := s.dependents.PlaceDependents(ctx, place.PrincipalID, place.ID)
	if err != nil || len(dependents) == 0 {
		return "", err
	}
	return "The place " + place.Name + " is used by " + strings.Join(dependents, "; ") + ". Remove or change those first, then delete the place.", nil
}

// act makes the change at once in a clean turn and proposes it otherwise.
func (s *Service) act(ctx context.Context, principalID string, call tools.Call, proposal approval.Proposal) (tools.Result, error) {
	text, err := s.proposer.Act(ctx, principalID, proposal)
	if err != nil {
		if approval.Refused(err) {
			return placeError(err.Error()), nil
		}
		s.logger.WarnContext(ctx, "place.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return placeError("the change could not be recorded"), nil
	}
	return tools.Result{Text: text}, nil
}

func (s *Service) ActionTools() []string {
	return []string{toolPlaceSave, toolPlaceUpdate, toolPlaceDelete}
}

// ExecuteApproved saves, changes, or deletes the confirmed place. The approval's own ID becomes the
// place ID, so a second execution cannot save a second place.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	switch record.Tool {
	case toolPlaceSave:
		var confirmed confirmedPlace
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed place could not be read; nothing was saved."}
		}
		fix, refusal, err := s.proposedFix(ctx, record.PrincipalID, confirmed)
		if err != nil {
			return approval.Outcome{}, err
		}
		if refusal != "" {
			return approval.Outcome{}, &approval.ActionError{Reason: "location_gone", Text: refusal}
		}
		place, err := s.newPlace(record.ID, record.PrincipalID, confirmed.Name, confirmed.RadiusM, fix)
		if err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "That place is not valid (" + err.Error() + "); nothing was saved."}
		}
		created, err := s.repository.CreatePlace(ctx, place, MaximumPlacesPerPrincipal)
		switch {
		case errors.Is(err, ErrTooManyPlaces):
			return approval.Outcome{}, &approval.ActionError{Reason: "too_many", Text: tooManyPlacesText() + " Nothing was saved."}
		case errors.Is(err, ErrPlaceNameTaken):
			return approval.Outcome{}, &approval.ActionError{Reason: "name_taken", Text: "Another place is already called " + place.Name + "; nothing was saved."}
		case err != nil:
			return approval.Outcome{}, err
		}
		if created {
			s.logger.InfoContext(ctx, "place.created", "principal_id", record.PrincipalID, "place_id", record.ID)
		}
		undo, err := s.placeUndoPayload(ctx, record.PrincipalID, record.ID, nil)
		if err != nil {
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: fmt.Sprintf("Saved the place %s, radius %d m.", place.Name, place.RadiusM), Undo: undo}, nil
	case toolPlaceUpdate:
		return s.executeUpdate(ctx, record)
	case toolPlaceDelete:
		var confirmed confirmedPlaceDelete
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed action could not be read; nothing was deleted."}
		}
		place, deletedAt, err := s.deleteUnused(ctx, record.PrincipalID, confirmed.PlaceID, " Nothing was deleted.")
		if err != nil {
			return approval.Outcome{}, err
		}
		// The place is kept, unseen, while it can be undone, so the Undo names it and its deletion
		// rather than carrying its point.
		undo, err := json.Marshal(placeUndo{PlaceID: place.ID, DeletedAt: deletedAt})
		if err != nil {
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: "Deleted the place " + place.Name + ".", Undo: undo}, nil
	default:
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: "That action is no longer available."}
	}
}

func (s *Service) executeUpdate(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed confirmedPlaceUpdate
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed change could not be read; nothing was changed."}
	}
	current, found, err := s.repository.LoadPlace(ctx, record.PrincipalID, confirmed.PlaceID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "That place no longer exists; nothing was changed."}
	}
	stale := &approval.ActionError{Reason: "stale", Text: "That place changed after this was proposed; nothing was changed. Ask again to change it."}
	if !current.UpdatedAt.Equal(confirmed.UpdatedAt) {
		return approval.Outcome{}, stale
	}
	name, err := normalizePlaceName(confirmed.Name)
	if err == nil {
		err = checkRadius(confirmed.RadiusM)
	}
	if err != nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "That change is not valid (" + err.Error() + "); nothing was changed."}
	}
	next := current
	next.Name, next.RadiusM, next.UpdatedAt = name, confirmed.RadiusM, s.now()
	updated, err := s.updatePlace(ctx, next, current.UpdatedAt)
	switch {
	case errors.Is(err, ErrPlaceNameTaken):
		return approval.Outcome{}, &approval.ActionError{Reason: "name_taken", Text: "Another place is already called " + name + "; nothing was changed."}
	case err != nil:
		return approval.Outcome{}, err
	case !updated:
		return approval.Outcome{}, stale
	}
	s.logger.InfoContext(ctx, "place.updated", "principal_id", record.PrincipalID, "place_id", current.ID)
	undo, err := s.placeUndoPayload(ctx, record.PrincipalID, current.ID, &current)
	if err != nil {
		return approval.Outcome{}, err
	}
	return approval.Outcome{Text: fmt.Sprintf("Changed. The place is now %s, radius %d m.", name, confirmed.RadiusM), Undo: undo}, nil
}

// proposedFix is the shared position a confirmed place was proposed from, as it is now: the same
// pin or venue, or the latest position of the same live share. A refusal is text for the user.
func (s *Service) proposedFix(ctx context.Context, principalID string, confirmed confirmedPlace) (Fix, string, error) {
	fix, found, err := s.Current(ctx, principalID)
	if err != nil {
		return Fix{}, "", err
	}
	same := found && fix.UpdatedAt.Equal(confirmed.SharedAt)
	if confirmed.LiveMessageID != 0 {
		same = found && fix.Source == SourceLive && fix.LiveMessageID == confirmed.LiveMessageID
	}
	if !same {
		return Fix{}, "The location this place was to be saved from is no longer shared; share it again and ask once more. Nothing was saved.", nil
	}
	return fix, "", nil
}

// newPlace is a place to store from a shared position, checked again as it is stored.
func (s *Service) newPlace(id, principalID, name string, radius int, fix Fix) (Place, error) {
	name, err := normalizePlaceName(name)
	if err != nil {
		return Place{}, err
	}
	if err := checkRadius(radius); err != nil {
		return Place{}, err
	}
	now := s.now()
	return Place{ID: id, PrincipalID: principalID, Name: name, Latitude: fix.Latitude, Longitude: fix.Longitude, RadiusM: radius, CreatedAt: now, UpdatedAt: now}, nil
}

// updatePlace changes a place's name or radius while no fix is stepping its presence, so a step
// read with the old radius cannot store its state after the new radius forgot it.
func (s *Service) updatePlace(ctx context.Context, place Place, expectedUpdatedAt time.Time) (bool, error) {
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	return s.repository.UpdatePlace(ctx, place, expectedUpdatedAt)
}

// deleteUnused deletes the principal's place unless a watch or an anchored job uses it, checking
// and deleting while nothing that names it can be stored; refusals end with suffix. It returns the
// place and when it was deleted.
func (s *Service) deleteUnused(ctx context.Context, principalID, placeID, suffix string) (Place, time.Time, error) {
	s.placeUse.Lock()
	defer s.placeUse.Unlock()
	place, found, err := s.repository.LoadPlace(ctx, principalID, placeID)
	if err != nil {
		return Place{}, time.Time{}, err
	}
	if !found {
		return Place{}, time.Time{}, &approval.ActionError{Reason: "not_found", Text: "That place no longer exists."}
	}
	if refusal, err := s.inUse(ctx, place); err != nil || refusal != "" {
		if err != nil {
			return Place{}, time.Time{}, err
		}
		return Place{}, time.Time{}, &approval.ActionError{Reason: "in_use", Text: refusal + suffix}
	}
	now := s.now()
	deleted, err := s.repository.DeletePlace(ctx, principalID, place.ID, place.UpdatedAt, now)
	if err != nil {
		return Place{}, time.Time{}, err
	}
	if !deleted {
		return Place{}, time.Time{}, &approval.ActionError{Reason: "stale", Text: "That place changed meanwhile; nothing was deleted. Ask again to delete it."}
	}
	s.logger.InfoContext(ctx, "place.deleted", "principal_id", principalID, "place_id", place.ID)
	return place, now, nil
}

func sourceName(fix Fix) string {
	switch fix.Source {
	case SourceLive:
		return "live location"
	case SourceVenue:
		return "venue"
	}
	return "location pin"
}

func tooManyPlacesText() string {
	return fmt.Sprintf("You already have %d places, which is the most allowed; delete one first.", MaximumPlacesPerPrincipal)
}

func placeError(text string) tools.Result {
	return tools.Result{Text: text, IsError: true}
}
