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
	var writable []string
	for _, root := range status.Roots {
		if root.Writable {
			writable = append(writable, root.Name)
		}
	}
	sort.Strings(writable)

	destination := "a folder is required, because this user has no default one"
	if status.DefaultFolder != "" {
		destination = "leave it out and the file goes to " + status.DefaultFolder
	}
	return "Saving to the NAS is on, and every save needs the user's confirmation: nas_save_attachment only proposes to file a photo or document the user already sent in this conversation. " +
		"Give folder only when the user says where the file belongs, such as a folder for finances; " + destination + ". " +
		"Only these folders can be saved to: " + strings.Join(writable, ", ") + ". Everything else on the NAS is read-only. " +
		"After your reply the user sees the exact destination with Confirm and Reject buttons, so describe what you proposed and never claim it was saved. " +
		"Every proposal you make in this turn is shown to the user together and confirmed or rejected as one batch, so propose every save the request needs in this turn, once each, and withdraw a wrong one with proposal_withdraw; a proposal in a later turn replaces a batch still waiting. " +
		"A save can only add a new file: a name already in use is refused rather than replaced, and nothing already on the NAS can be changed or removed. " +
		"Use list_attachments to find the id of the file the user sent."
}

var writeDefinitions = []tools.Definition{
	{
		Name:        toolSaveAttachment,
		Description: "Propose saving a file or photo the user sent in this conversation to a folder on the NAS. The user must confirm, and an existing name is refused rather than replaced.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"attachment_id":{"type":"string","description":"The id of the file the user sent, as listed by list_attachments."},` +
			`"folder":{"type":"string","description":"Where the user asked for it, such as home/Documents/Finance. Omit to use this user's default folder. Missing folders are created."},` +
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
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
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

	if _, taken, err := entryOf(ctx, connection, location); err != nil {
		return s.toolFailure(ctx, principalID, call.Name, err)
	} else if taken {
		return tools.Result{Text: failureText(ErrorExists), IsError: true}, nil
	}
	missing, err := missingDirectories(ctx, connection, location)
	if err != nil {
		return s.toolFailure(ctx, principalID, call.Name, err)
	}

	canonical, err := json.Marshal(confirmedSave{AttachmentID: record.ID, Path: location.String()})
	if err != nil {
		return tools.Result{}, err
	}
	proposal := approval.Proposal{Tool: toolSaveAttachment, Arguments: canonical, Summary: saveSummary(record, location, missing), Intent: approval.Intent{Verb: "save", TargetKind: "file", TargetName: path.Base(location.Path)}}
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
	if !writableRoot(status.Roots, location.Root) {
		return Location{}, fmt.Errorf("%s is read-only; save to one of the folders that can be written to", location.Root)
	}

	name := safeName(record.FileName)
	if chosen := strings.TrimSpace(arguments.Name); chosen != "" {
		if err := validateSegment(chosen); err != nil {
			return Location{}, err
		}
		name = chosen
	}
	return location.Child(name), nil
}

func writableRoot(roots []Root, name string) bool {
	for _, root := range roots {
		if root.Name == name {
			return root.Writable
		}
	}
	return false
}

func saveSummary(record attachment.Record, location Location, missing []Location) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Save %q (%s) to the NAS as %s", record.FileName, formatSize(record.SizeBytes), location)
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

	for _, directory := range location.Ancestors() {
		if err := connection.MakeDirectory(ctx, directory); err != nil {
			if classify(err).Class == ErrorExists {
				continue
			}
			return approval.Outcome{}, actionErrorFor(err)
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

// missingDirectories lists the folders a save would have to create, outermost first. The walk
// stops at the first missing one because everything below it is missing too.
func missingDirectories(ctx context.Context, connection Session, location Location) ([]Location, error) {
	var missing []Location
	for _, directory := range location.Ancestors() {
		if len(missing) > 0 {
			missing = append(missing, directory)
			continue
		}
		found, exists, err := entryOf(ctx, connection, directory)
		if err != nil {
			return nil, err
		}
		if !exists {
			missing = append(missing, directory)
			continue
		}
		if !found.Directory {
			return nil, &Error{Class: ErrorExists}
		}
	}
	return missing, nil
}
