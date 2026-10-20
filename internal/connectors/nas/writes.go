package nas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const toolSaveAttachment = "nas_save_attachment"

func writeGuidance(status Status) string {
	var own, shared, creating []string
	for _, root := range status.Roots {
		if !root.Writable {
			continue
		}
		if root.Shared {
			shared = append(shared, root.Name)
		} else {
			own = append(own, root.Name)
		}
		if root.CreateFolders {
			creating = append(creating, root.Name)
		}
	}
	sort.Strings(own)
	sort.Strings(shared)
	sort.Strings(creating)

	destination := "a folder is required, because this user has no default one"
	fallback := "leave folder out and tell the user why"
	if status.DefaultFolder != "" {
		destination = "leave it out and the file goes to " + status.DefaultFolder
		fallback = "leave folder out so the file goes to " + status.DefaultFolder + ", and tell the user why"
	}
	return "Saving to the NAS is on, and every save needs the user's confirmation: nas_save_attachment only proposes to file a photo or document the user already sent in this conversation. " +
		"The user's inbox, or \"my inbox\", is their own inbox and never a shared folder. Use a folder shared with the household only when the user says shared, household, family, or both of us. " +
		"Give folder when the user says where the file belongs, such as a folder for finances, or when filing it where it belongs as described next; otherwise " + destination + ". " +
		"When the user asks to file something where it belongs without naming a folder, call nas_list_folders on the user's own writable folders, choose the best-fitting existing folder from its name and the file the user sent, and never invent a folder; if nothing fits, " + fallback + ". " +
		"Folders this user can save to: " + describeFolders(own, shared) + ". " +
		"New folders can be created only in " + describeCreating(creating) + "; elsewhere the folder must already exist. Everything else on the NAS is read-only. " +
		"After your reply the user sees the exact destination with Confirm and Reject buttons, so describe what you proposed and never claim it was saved. " +
		"Every proposal you make in this turn is shown to the user together and confirmed or rejected as one batch, so propose every save the request needs in this turn, once each, and withdraw a wrong one with proposal_withdraw; a proposal in a later turn replaces a batch still waiting. " +
		"A save can only add a new file: a name already in use is refused rather than replaced, and nothing already on the NAS can be changed or removed. " +
		"Use list_attachments to find the id of the file the user sent."
}

func describeFolders(own, shared []string) string {
	var parts []string
	if len(own) > 0 {
		parts = append(parts, strings.Join(own, ", ")+" (their own)")
	}
	if len(shared) > 0 {
		parts = append(parts, strings.Join(shared, ", ")+" (shared with the household)")
	}
	return strings.Join(parts, "; ")
}

func describeCreating(names []string) string {
	if len(names) == 0 {
		return "no folder"
	}
	return strings.Join(names, ", ")
}

var writeDefinitions = []tools.Definition{
	{
		Name:        toolSaveAttachment,
		Description: "Propose saving a file or photo the user sent in this conversation to a folder on the NAS. The user must confirm, and an existing name is refused rather than replaced.",
		Risk:        tools.RiskWrite,
		Activity:    tools.ActivityFiles,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"attachment_id":{"type":"string","description":"The id of the file the user sent, as listed by list_attachments."},` +
			`"folder":{"type":"string","description":"An existing folder, for example one from nas_list_folders such as home/Documents. Omit to use this user's default folder. Missing folders are created only in the folders the guidance names."},` +
			`"name":{"type":"string","description":"The file name to save under. Omit to keep the name the user sent it with."}},` +
			`"required":["attachment_id"],"additionalProperties":false}`),
	},
}

// Proposer is the core approval service as the connector sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
}

// Attachments is the attachment store as the connector sees it: files the user already sent in
// this conversation, addressed by the id the model saw there.
type Attachments interface {
	Describe(ctx context.Context, principalID, attachmentID string) (attachment.Record, bool, error)
	Content(ctx context.Context, principalID, attachmentID string) (attachment.Record, []byte, bool, error)
}

var _ approval.Executor = (*Service)(nil)

func (s *Service) ActionTools() []string {
	return []string{toolSaveAttachment}
}

// saveArguments is what the model sends. The confirmed record stores the resolved path instead,
// so the destination the user confirmed cannot be re-derived differently at execution.
type saveArguments struct {
	AttachmentID string `json:"attachment_id"`
	Folder       string `json:"folder"`
	Name         string `json:"name"`
}

type confirmedSave struct {
	AttachmentID string `json:"attachment_id"`
	Path         string `json:"path"`
}

// executeSave turns a save call into a proposal. It resolves the destination, refuses a name
// already in use, and works out which folders the save would create, so the confirmation the user
// reads is the whole truth about what pressing Confirm does.
func (s *Service) executeSave(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	status, err := s.Status(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	if !status.Connected {
		return tools.Result{Text: NoAccessText, IsError: true}, nil
	}
	// The broker only offers this tool while the switch is on, but the grant is checked here too
	// so the connector never proposes a save the user has not allowed.
	if !status.CanSave || !status.WritesEnabled {
		return tools.Result{Text: "Saving to the NAS is off for this user.", IsError: true}, nil
	}

	var arguments saveArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	record, found, err := s.attachments.Describe(ctx, principalID, arguments.AttachmentID)
	if err != nil {
		return tools.Result{}, err
	}
	if !found {
		return tools.Result{Text: "No file with that id is available in this conversation. Use list_attachments to see what the user sent.", IsError: true}, nil
	}
	location, err := s.destination(status, arguments, record)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}

	connection, allowed, err := s.session(ctx, principalID)
	if err != nil {
		return s.toolFailure(ctx, principalID, call.Name, err)
	}
	if !allowed {
		return tools.Result{Text: NoAccessText, IsError: true}, nil
	}
	defer connection.Close()

	location, missing, err := resolveFolders(ctx, connection, location)
	if err != nil {
		return s.toolFailure(ctx, principalID, call.Name, err)
	}
	root, _ := rootNamed(status.Roots, location.Root)
	if len(missing) > 0 && !root.CreateFolders {
		return tools.Result{Text: missingFolderText(missing[0], location.Root, status.DefaultFolder), IsError: true}, nil
	}
	if _, taken, err := entryOf(ctx, connection, location); err != nil {
		return s.toolFailure(ctx, principalID, call.Name, err)
	} else if taken {
		return tools.Result{Text: failureText(ErrorExists), IsError: true}, nil
	}

	canonical, err := json.Marshal(confirmedSave{AttachmentID: record.ID, Path: location.String()})
	if err != nil {
		return tools.Result{}, err
	}
	proposal := approval.Proposal{Tool: toolSaveAttachment, Arguments: canonical, Summary: saveSummary(record, location, missing, root.Shared), Intent: approval.Intent{Verb: "save", TargetKind: "file", TargetName: path.Base(location.Path)}}
	if _, err := s.proposer.Propose(ctx, principalID, proposal); err != nil {
		if approval.Refused(err) {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		s.logger.WarnContext(ctx, "nas.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return tools.Result{Text: "the proposal could not be recorded", IsError: true}, nil
	}
	return tools.Result{Text: approval.ProposedText}, nil
}

// destination resolves where the file would land: the folder the user named, or this principal's
// default, plus the name it was sent with unless the model was told another.
func (s *Service) destination(status Status, arguments saveArguments, record attachment.Record) (Location, error) {
	folder := strings.TrimSpace(arguments.Folder)
	var location Location
	var err error
	switch {
	case folder != "":
		location, err = parseLocation(folder)
	case status.DefaultFolder != "":
		location, err = parseLocation(status.DefaultFolder)
	default:
		return Location{}, fmt.Errorf("this user has no default folder, so name the folder to save into")
	}
	if err != nil {
		return Location{}, err
	}
	if root, _ := rootNamed(status.Roots, location.Root); !root.Writable {
		return Location{}, fmt.Errorf("%s is read-only; save to one of the folders that can be written to", location.Root)
	}
	for _, segment := range strings.Split(location.Path, "/") {
		if err := refuseSystemName(segment); err != nil {
			return Location{}, err
		}
	}

	name := safeName(record.FileName)
	if chosen := strings.TrimSpace(arguments.Name); chosen != "" {
		if strings.ContainsAny(chosen, `/\`) {
			return Location{}, fmt.Errorf("name is the file name only and cannot contain / or \\; put the folders in folder")
		}
		if err := validateSegment(chosen); err != nil {
			return Location{}, err
		}
		name = chosen
	}
	if err := refuseSystemName(name); err != nil {
		return Location{}, err
	}
	return location.Child(name), nil
}

func rootNamed(roots []Root, name string) (Root, bool) {
	for _, root := range roots {
		if root.Name == name {
			return root, true
		}
	}
	return Root{}, false
}

// refuseSystemName keeps a save out of the folders a NAS keeps for itself, such as snapshots and
// recycle bins, and out of hidden names.
func refuseSystemName(segment string) error {
	if segment != "" && strings.ContainsRune(".@#", rune(segment[0])) {
		return fmt.Errorf("%q starts with %q, which is reserved for system and hidden items; choose another name", segment, segment[:1])
	}
	return nil
}

func missingFolderText(missing Location, root, defaultFolder string) string {
	text := fmt.Sprintf("%s does not exist, and new folders cannot be created in %s; choose an existing folder from nas_list_folders", missing, root)
	if defaultFolder != "" {
		text += ", or leave folder out to save to " + defaultFolder
	}
	return text + "."
}

func saveSummary(record attachment.Record, location Location, missing []Location, shared bool) string {
	owner := "your"
	if shared {
		owner = "the shared"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "Save %q (%s) to %s NAS folder as %s", record.FileName, formatSize(record.SizeBytes), owner, location)
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for _, directory := range missing {
			names = append(names, directory.String())
		}
		fmt.Fprintf(&builder, "\nCreates the folder %s", strings.Join(names, ", "))
	}
	builder.WriteString("\nNothing already on the NAS is changed or removed.")
	return builder.String()
}

// ExecuteApproved writes the confirmed save exactly once. Every guard is checked again here,
// because the grant, the access, and the attachment can all disappear between the proposal and
// the press.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	if record.Tool != toolSaveAttachment || s.proposer == nil || s.attachments == nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: "That action is no longer available; nothing was saved."}
	}
	var confirmed confirmedSave
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed action could not be read; nothing was saved."}
	}
	location, err := parseLocation(confirmed.Path)
	if err != nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed action could not be read; nothing was saved."}
	}
	enabled, err := s.writesEnabled(ctx, record.PrincipalID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !enabled {
		return approval.Outcome{}, &approval.ActionError{Reason: "writes_disabled", Text: "Saving to the NAS is off; nothing was saved."}
	}
	_, data, found, err := s.attachments.Content(ctx, record.PrincipalID, confirmed.AttachmentID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, &approval.ActionError{Reason: "attachment_gone", Text: "That file is no longer in this conversation; nothing was saved."}
	}

	connection, allowed, err := s.session(ctx, record.PrincipalID)
	if err != nil {
		return approval.Outcome{}, actionErrorFor(err)
	}
	if !allowed {
		return approval.Outcome{}, &approval.ActionError{Reason: "no_access", Text: "This user no longer has NAS access; nothing was saved."}
	}
	defer connection.Close()

	// The live session decides whether folders may be created, not the proposal, so a root that
	// stopped allowing it fails the save as a missing folder.
	if root, _ := rootNamed(connection.Roots(), location.Root); root.CreateFolders {
		for _, directory := range location.Ancestors() {
			if err := connection.MakeDirectory(ctx, directory); err != nil {
				if classify(err).Class == ErrorExists {
					continue
				}
				return approval.Outcome{}, actionErrorFor(err)
			}
		}
	}
	if err := connection.Create(ctx, location, data); err != nil {
		return approval.Outcome{}, actionErrorFor(err)
	}
	s.logger.InfoContext(ctx, "nas.action.executed", "principal_id", record.PrincipalID, "tool", record.Tool)
	return approval.Outcome{Text: fmt.Sprintf("Saved to %s.", location)}, nil
}

func actionErrorFor(err error) error {
	var nasErr *Error
	if !errors.As(err, &nasErr) {
		return err
	}
	switch nasErr.Class {
	case ErrorExists:
		return &approval.ActionError{Reason: "exists", Text: "A file with that name now exists; nothing was replaced."}
	case ErrorDenied:
		return &approval.ActionError{Reason: "denied", Text: "That folder is read-only; nothing was saved."}
	case ErrorNotFound:
		return &approval.ActionError{Reason: "not_found", Text: "The NAS has no such folder; nothing was saved."}
	default:
		return &approval.ActionError{Reason: "unavailable", Text: "The NAS is unavailable right now; nothing was saved."}
	}
}

// entryOf reports one location's own entry by listing its parent, so a session needs no stat call
// and a missing parent simply reads as absent.
func entryOf(ctx context.Context, connection Session, location Location) (Entry, bool, error) {
	entries, err := connection.List(ctx, location.Parent())
	if err != nil {
		if classify(err).Class == ErrorNotFound {
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	// The comparison ignores case even though the NAS filesystem does not, so a save cannot add a
	// second file whose name differs from an existing one only by case. Refusing is the safe
	// direction: the user asked for a new file, not for two that look alike over SMB.
	name := location.Name()
	for _, found := range entries {
		if strings.EqualFold(found.Name, name) {
			return found, true, nil
		}
	}
	return Entry{}, false, nil
}

// resolveFolders walks the destination's folders against the disk. Each existing folder takes the
// exact name found there, so the stored path and the confirmation match the disk even when the
// model wrote different capitals. The folders that do not exist, outermost first, are returned as
// they would be created; the walk stops looking at the first missing one because everything below
// it is missing too.
func resolveFolders(ctx context.Context, connection Session, location Location) (Location, []Location, error) {
	segments := strings.Split(location.Path, "/")
	resolved := Location{Root: location.Root}
	var missing []Location
	for _, segment := range segments[:len(segments)-1] {
		if len(missing) == 0 {
			entries, err := connection.List(ctx, resolved)
			if err != nil && classify(err).Class != ErrorNotFound {
				return Location{}, nil, err
			}
			if found, exists := matchEntry(entries, segment); exists {
				if !found.Directory {
					return Location{}, nil, &Error{Class: ErrorExists}
				}
				resolved = resolved.Child(found.Name)
				continue
			}
		}
		resolved = resolved.Child(segment)
		missing = append(missing, resolved)
	}
	return resolved.Child(segments[len(segments)-1]), missing, nil
}

// matchEntry prefers the exact name and falls back to a different capitalization.
func matchEntry(entries []Entry, name string) (Entry, bool) {
	var folded *Entry
	for index, found := range entries {
		if found.Name == name {
			return found, true
		}
		if folded == nil && strings.EqualFold(found.Name, name) {
			folded = &entries[index]
		}
	}
	if folded != nil {
		return *folded, true
	}
	return Entry{}, false
}
