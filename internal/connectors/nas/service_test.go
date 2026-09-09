package nas_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/connectors/nas"
	"github.com/vladar107/home-ai-assistant/internal/delivery"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// recorder is the approval service as the connector sees it, so a test can prove that a refused
// save records no proposal at all.
type recorder struct {
	proposals []approval.Proposal
}

func (r *recorder) Propose(_ context.Context, principalID string, proposal approval.Proposal) (approval.Record, error) {
	r.proposals = append(r.proposals, proposal)
	return approval.Record{
		ID: fmt.Sprintf("approval-%d", len(r.proposals)), PrincipalID: principalID,
		Tool: proposal.Tool, Arguments: proposal.Arguments, Summary: proposal.Summary,
	}, nil
}

// sentFiles is the attachment store as the connector sees it.
type sentFiles struct {
	records map[string]attachment.Record
	data    map[string][]byte
}

func (f *sentFiles) Describe(_ context.Context, principalID, attachmentID string) (attachment.Record, bool, error) {
	record, found := f.records[attachmentID]
	if !found || record.PrincipalID != principalID {
		return attachment.Record{}, false, nil
	}
	return record, true, nil
}

func (f *sentFiles) Content(ctx context.Context, principalID, attachmentID string) (attachment.Record, []byte, bool, error) {
	record, found, err := f.Describe(ctx, principalID, attachmentID)
	if err != nil || !found {
		return attachment.Record{}, nil, false, err
	}
	return record, f.data[attachmentID], true, nil
}

func (f *sentFiles) add(principalID, id, name string, content []byte) {
	f.records[id] = attachment.Record{ID: id, PrincipalID: principalID, FileName: name, SizeBytes: int64(len(content))}
	f.data[id] = content
}

// outbox is the delivery service as the connector sees it. It closes the file the way delivery
// does, so a test would notice the connector handing over something it had already closed.
type outbox struct {
	mu        sync.Mutex
	documents []sentDocument
	err       error
}

type sentDocument struct {
	principalID string
	name        string
	data        []byte
}

func (o *outbox) SendFile(_ context.Context, principalID string, file delivery.File) error {
	defer file.Body.Close()
	if o.err != nil {
		return o.err
	}
	data, err := io.ReadAll(file.Body)
	if err != nil {
		return err
	}
	if int64(len(data)) != file.Size {
		return fmt.Errorf("file %q reported %d bytes and carried %d", file.Name, file.Size, len(data))
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.documents = append(o.documents, sentDocument{principalID: principalID, name: file.Name, data: data})
	return nil
}

func (o *outbox) sent() []sentDocument {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]sentDocument(nil), o.documents...)
}

type fixture struct {
	t         *testing.T
	ctx       context.Context
	service   *nas.Service
	store     *sqlite.Store
	home      string
	inbox     string
	second    string
	proposals *recorder
	files     *sentFiles
	outbox    *outbox
	logs      *lockedBuffer
}

// newFixture builds the connector over real directories, which is what production uses: the
// store reads mounted directories, so there is nothing to stand in for.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	state, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if err := state.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}

	home, inbox, second := t.TempDir(), t.TempDir(), t.TempDir()
	logs := &lockedBuffer{}
	proposals := &recorder{}
	files := &sentFiles{records: map[string]attachment.Record{}, data: map[string][]byte{}}
	chat := &outbox{}
	service, err := nas.NewService(nas.Config{Roots: []nas.PrincipalRoots{
		{
			PrincipalID: "alice",
			Roots: []nas.RootPath{
				{Name: "home", Path: home},
				{Name: "inbox", Path: inbox, Writable: true},
			},
			DefaultWrite: "inbox",
		},
		{
			PrincipalID: "bob",
			Roots:       []nas.RootPath{{Name: "home", Path: second}},
		},
	}}, state, proposals, files, chat, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, ctx: ctx, service: service, store: state, home: home, inbox: inbox, second: second, proposals: proposals, files: files, outbox: chat, logs: logs}
}

func (f *fixture) write(directory, path, content string) {
	f.t.Helper()
	full := filepath.Join(directory, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(directory, path string) (string, bool) {
	f.t.Helper()
	content, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(path)))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return string(content), true
}

func (f *fixture) execute(name, arguments string) tools.Result {
	f.t.Helper()
	result, err := f.service.Execute(f.ctx, "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		f.t.Fatal(err)
	}
	// The model must never learn where the roots actually live on the NAS.
	if strings.Contains(result.Text, f.home) || strings.Contains(result.Text, f.inbox) {
		f.t.Fatalf("result exposes a host path: %+v", result)
	}
	return result
}

func (f *fixture) contribute() tools.Contribution {
	f.t.Helper()
	contribution, err := f.service.Contribute(f.ctx, "alice")
	if err != nil {
		f.t.Fatal(err)
	}
	return contribution
}

func (f *fixture) enableSaving() {
	f.t.Helper()
	f.contribute()
	if status, err := f.service.SetWrites(f.ctx, "alice", true); err != nil || !status.WritesEnabled {
		f.t.Fatalf("enable saving = %+v %v", status, err)
	}
}

func (f *fixture) proposal() approval.Proposal {
	f.t.Helper()
	if len(f.proposals.proposals) == 0 {
		f.t.Fatal("no proposal was recorded")
	}
	return f.proposals.proposals[len(f.proposals.proposals)-1]
}

func (f *fixture) confirm(proposal approval.Proposal) (approval.Outcome, error) {
	f.t.Helper()
	return f.service.ExecuteApproved(f.ctx, approval.Record{PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments})
}

func names(contribution tools.Contribution) []string {
	var found []string
	for _, definition := range contribution.Definitions {
		found = append(found, definition.Name)
	}
	return found
}

func actionErrorOf(t *testing.T, err error) *approval.ActionError {
	t.Helper()
	var actionErr *approval.ActionError
	if !errors.As(err, &actionErr) {
		t.Fatalf("error = %v, want an approval.ActionError", err)
	}
	return actionErr
}

// Each principal sees the roots configured for them and nothing else, which is the whole of the
// per-user boundary now that one process serves both people.
func TestRootsAreScopedToThePrincipal(t *testing.T) {
	f := newFixture(t)
	f.write(f.home, "Documents/notes.md", "milk and bread")
	f.write(f.second, "Photos/private.md", "not for alice")
	f.contribute()

	roots := f.execute("nas_list_files", `{}`)
	if roots.IsError || !strings.Contains(roots.Text, "[folder, read-only] home") || !strings.Contains(roots.Text, "[folder, can be saved to] inbox") {
		t.Fatalf("roots = %+v", roots)
	}
	listing := f.execute("nas_list_files", `{"path":"home/Documents"}`)
	if listing.IsError || !strings.Contains(listing.Text, "[file] home/Documents/notes.md") {
		t.Fatalf("listing = %+v", listing)
	}
	// Bob's root is mounted in the same process; only the grant keeps it out of Alice's reach.
	unknown := f.execute("nas_list_files", `{"path":"second"}`)
	if !unknown.IsError || !strings.Contains(unknown.Text, "no folder or file at that path") {
		t.Fatalf("another principal's root = %+v", unknown)
	}
	if strings.Contains(f.logs.String(), f.second) {
		t.Fatal("a host path reached the logs")
	}
}

func TestBobSeesOnlyHisOwnRoot(t *testing.T) {
	f := newFixture(t)
	f.write(f.second, "Photos/holiday.md", "sunny")
	contribution, err := f.service.Contribute(f.ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	// Bob has no writable root, so saving is never offered to him; browsing, reading and sending are.
	if got := names(contribution); len(got) != 4 {
		t.Fatalf("bob's tools = %+v", got)
	}
	result, err := f.service.Execute(f.ctx, "bob", tools.Call{Name: "nas_list_files", Arguments: json.RawMessage(`{"path":"inbox"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("bob reached another principal's writable root: %+v", result)
	}
}

func TestPrincipalWithoutRootsGetsNothing(t *testing.T) {
	f := newFixture(t)
	contribution, err := f.service.Contribute(f.ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if len(contribution.Definitions) != 0 {
		t.Fatalf("an unconfigured principal was offered tools: %+v", names(contribution))
	}
	result, err := f.service.Execute(f.ctx, "carol", tools.Call{Name: "nas_list_files", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Text, "no NAS folders") {
		t.Fatalf("unconfigured principal's call = %+v", result)
	}
}

func TestReadingIsTextOnlyAndResumable(t *testing.T) {
	f := newFixture(t)
	f.write(f.home, "Documents/notes.md", "milk and bread")
	f.write(f.home, "Documents/long.md", strings.Repeat("a", 20<<10))
	f.write(f.home, "Documents/photo.jpg", "\xff\xd8\xff binary")
	f.write(f.home, "Documents/empty.md", "")
	f.contribute()

	if read := f.execute("nas_read_file", `{"path":"home/Documents/notes.md"}`); read.IsError ||
		!strings.Contains(read.Text, "milk and bread") || !strings.Contains(read.Text, "14 bytes") {
		t.Fatalf("read = %+v", read)
	}
	if binary := f.execute("nas_read_file", `{"path":"home/Documents/photo.jpg"}`); !binary.IsError ||
		!strings.Contains(binary.Text, "not a text file") {
		t.Fatalf("binary read = %+v", binary)
	}
	if empty := f.execute("nas_read_file", `{"path":"home/Documents/empty.md"}`); !empty.IsError ||
		!strings.Contains(empty.Text, "is empty") {
		t.Fatalf("empty read = %+v", empty)
	}
	first := f.execute("nas_read_file", `{"path":"home/Documents/long.md"}`)
	if first.IsError || !strings.Contains(first.Text, "continue with offset_bytes 12288") {
		t.Fatalf("truncated read = %+v", first)
	}
	if rest := f.execute("nas_read_file", `{"path":"home/Documents/long.md","offset_bytes":12288}`); rest.IsError ||
		strings.Contains(rest.Text, "continue with offset_bytes") {
		t.Fatalf("continued read = %+v", rest)
	}
	if folder := f.execute("nas_read_file", `{"path":"home/Documents"}`); !folder.IsError {
		t.Fatalf("reading a folder = %+v", folder)
	}
}

// A symlink planted inside a root must not become a way out of it.
func TestSymlinksCannotLeaveARoot(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("outside the root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.home, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(f.home, "secret.md")); err != nil {
		t.Fatal(err)
	}
	f.contribute()

	listing := f.execute("nas_list_files", `{"path":"home"}`)
	if listing.IsError || strings.Contains(listing.Text, "escape") || strings.Contains(listing.Text, "secret.md") {
		t.Fatalf("a symlink was listed: %+v", listing)
	}
	if through := f.execute("nas_read_file", `{"path":"home/escape/secret.md"}`); !through.IsError ||
		strings.Contains(through.Text, "outside the root") {
		t.Fatalf("a symlinked directory was followed out of the root: %+v", through)
	}
	if direct := f.execute("nas_read_file", `{"path":"home/secret.md"}`); !direct.IsError ||
		strings.Contains(direct.Text, "outside the root") {
		t.Fatalf("a symlinked file was followed out of the root: %+v", direct)
	}
}

func TestSearchMatchesNamesUnderAFolder(t *testing.T) {
	f := newFixture(t)
	f.write(f.home, "Documents/2026/invoice-january.pdf", "x")
	f.write(f.home, "Documents/2026/invoice-february.pdf", "x")
	f.write(f.home, "Documents/holiday.jpg", "x")
	f.contribute()

	result := f.execute("nas_search_files", `{"query":"invoice","path":"home"}`)
	if result.IsError || !strings.Contains(result.Text, "invoice-january.pdf") || !strings.Contains(result.Text, "invoice-february.pdf") {
		t.Fatalf("search = %+v", result)
	}
	if strings.Contains(result.Text, "holiday.jpg") {
		t.Fatalf("search returned an unmatched name: %+v", result)
	}
	if empty := f.execute("nas_search_files", `{"query":"taxes","path":"home"}`); empty.IsError ||
		!strings.Contains(empty.Text, "(none)") {
		t.Fatalf("empty search = %+v", empty)
	}
}

func TestSavingIsOffUntilTheUserSwitchesItOn(t *testing.T) {
	f := newFixture(t)
	f.files.add("alice", "att1", "scan.pdf", []byte("scanned bytes"))

	for _, name := range names(f.contribute()) {
		if name == "nas_save_attachment" {
			t.Fatal("the save tool was offered before saving was switched on")
		}
	}
	if result := f.execute("nas_save_attachment", `{"attachment_id":"att1"}`); !result.IsError {
		t.Fatalf("save without the grant = %+v", result)
	}

	f.enableSaving()
	after := f.contribute()
	if got := names(after); len(got) != 5 {
		t.Fatalf("contribution after enabling = %+v", got)
	}
	if !strings.Contains(after.Guidance, "the file goes to inbox") || !strings.Contains(after.Guidance, "Only these folders can be saved to: inbox") {
		t.Fatalf("guidance does not describe the destinations: %s", after.Guidance)
	}
}

// With no folder named the file lands in the user's default; the name it was sent with is kept.
func TestSaveDefaultsToTheUsersOwnFolder(t *testing.T) {
	f := newFixture(t)
	f.files.add("alice", "att1", "scan.pdf", []byte("scanned bytes"))
	f.enableSaving()

	if result := f.execute("nas_save_attachment", `{"attachment_id":"att1"}`); result.IsError ||
		!strings.Contains(result.Text, "awaiting the user's confirmation") {
		t.Fatalf("proposal = %+v", result)
	}
	proposal := f.proposal()
	if !strings.Contains(proposal.Summary, `Save "scan.pdf"`) || !strings.Contains(proposal.Summary, "as inbox/scan.pdf") {
		t.Fatalf("summary = %q", proposal.Summary)
	}
	if _, found := f.read(f.inbox, "scan.pdf"); found {
		t.Fatal("a proposal wrote a file before it was confirmed")
	}
	if outcome, err := f.confirm(proposal); err != nil || !strings.Contains(outcome.Text, "inbox/scan.pdf") {
		t.Fatalf("confirmed save = %+v %v", outcome, err)
	}
	if content, found := f.read(f.inbox, "scan.pdf"); !found || content != "scanned bytes" {
		t.Fatalf("saved file = %q %v", content, found)
	}
}

// The user can send a document somewhere specific, and folders on the way are created with it.
func TestSaveIntoAFolderTheUserNames(t *testing.T) {
	f := newFixture(t)
	f.files.add("alice", "att1", "statement.pdf", []byte("bank statement"))
	f.enableSaving()

	result := f.execute("nas_save_attachment", `{"attachment_id":"att1","folder":"inbox/Finance/2026","name":"september.pdf"}`)
	if result.IsError {
		t.Fatalf("proposal = %+v", result)
	}
	proposal := f.proposal()
	if !strings.Contains(proposal.Summary, "as inbox/Finance/2026/september.pdf") ||
		!strings.Contains(proposal.Summary, "Creates the folder inbox/Finance, inbox/Finance/2026") {
		t.Fatalf("summary = %q", proposal.Summary)
	}
	if _, err := f.confirm(proposal); err != nil {
		t.Fatal(err)
	}
	if content, found := f.read(f.inbox, "Finance/2026/september.pdf"); !found || content != "bank statement" {
		t.Fatalf("saved file = %q %v", content, found)
	}
}

func TestSaveRefusesReadOnlyFoldersAndExistingNames(t *testing.T) {
	f := newFixture(t)
	f.files.add("alice", "att1", "scan.pdf", []byte("scanned bytes"))
	f.write(f.inbox, "taken.pdf", "an existing file")
	f.enableSaving()

	readOnly := f.execute("nas_save_attachment", `{"attachment_id":"att1","folder":"home/Documents"}`)
	if !readOnly.IsError || !strings.Contains(readOnly.Text, "read-only") {
		t.Fatalf("save into a read-only root = %+v", readOnly)
	}
	taken := f.execute("nas_save_attachment", `{"attachment_id":"att1","name":"taken.pdf"}`)
	if !taken.IsError || !strings.Contains(taken.Text, "already exists") {
		t.Fatalf("save over an existing name = %+v", taken)
	}
	if len(f.proposals.proposals) != 0 {
		t.Fatalf("a refused save recorded %d proposal(s)", len(f.proposals.proposals))
	}
	if content, _ := f.read(f.inbox, "taken.pdf"); content != "an existing file" {
		t.Fatalf("an existing file was modified: %q", content)
	}
}

// The read-only mount is the outer guarantee: a confirmed save still fails if the directory
// itself refuses the write, whatever this package believed.
func TestAReadOnlyDirectoryRefusesAConfirmedSave(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores directory permissions")
	}
	f := newFixture(t)
	f.files.add("alice", "att1", "scan.pdf", []byte("scanned bytes"))
	f.enableSaving()
	if result := f.execute("nas_save_attachment", `{"attachment_id":"att1"}`); result.IsError {
		t.Fatalf("proposal = %+v", result)
	}
	proposal := f.proposal()
	if err := os.Chmod(f.inbox, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.inbox, 0o755) })

	_, err := f.confirm(proposal)
	if actionErr := actionErrorOf(t, err); actionErr.Reason != "denied" {
		t.Fatalf("confirmed save into a read-only directory = %+v", actionErr)
	}
	if _, found := f.read(f.inbox, "scan.pdf"); found {
		t.Fatal("a read-only directory accepted a write")
	}
}

func TestSwitchingSavingOffStopsAConfirmedSave(t *testing.T) {
	f := newFixture(t)
	f.files.add("alice", "att1", "scan.pdf", []byte("scanned bytes"))
	f.enableSaving()
	if result := f.execute("nas_save_attachment", `{"attachment_id":"att1"}`); result.IsError {
		t.Fatalf("proposal = %+v", result)
	}
	proposal := f.proposal()
	if _, err := f.service.SetWrites(f.ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	_, err := f.confirm(proposal)
	if actionErr := actionErrorOf(t, err); actionErr.Reason != "writes_disabled" {
		t.Fatalf("save after the switch went off = %+v", actionErr)
	}
	if _, found := f.read(f.inbox, "scan.pdf"); found {
		t.Fatal("a save ran after the grant was withdrawn")
	}
}

func TestSaveRefusesAnAttachmentFromAnotherConversation(t *testing.T) {
	f := newFixture(t)
	f.files.add("bob", "att-bob", "bob.pdf", []byte("bob's file"))
	f.enableSaving()
	result := f.execute("nas_save_attachment", `{"attachment_id":"att-bob"}`)
	if !result.IsError || !strings.Contains(result.Text, "No file with that id") {
		t.Fatalf("save of another principal's attachment = %+v", result)
	}
}

// Sending is what the user asked for when they want the file itself, and it must not depend on
// reading: the bytes go from the mounted directory to the chat without passing through the model.
func TestSendingAFileDeliversItsBytesUnread(t *testing.T) {
	f := newFixture(t)
	f.write(f.home, "Documents/Payslip.pdf", "%PDF-1.7 payslip")
	f.contribute()

	result := f.execute("nas_send_file", `{"path":"home/Documents/Payslip.pdf"}`)
	if result.IsError || !strings.Contains(result.Text, "Sent home/Documents/Payslip.pdf") {
		t.Fatalf("send = %+v", result)
	}
	sent := f.outbox.sent()
	if len(sent) != 1 {
		t.Fatalf("documents sent = %+v", sent)
	}
	if sent[0].principalID != "alice" || sent[0].name != "Payslip.pdf" || string(sent[0].data) != "%PDF-1.7 payslip" {
		t.Fatalf("document = %+v", sent[0])
	}
	// The content reaches Telegram, never the model.
	if strings.Contains(result.Text, "%PDF") {
		t.Fatalf("the file's content reached the model: %+v", result)
	}
}

// A file is addressed exactly as it is for reading, so every refusal that protects reading
// protects sending too.
func TestSendingStaysInsideTheGrant(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.pdf"), []byte("outside the root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.pdf"), filepath.Join(f.home, "secret.pdf")); err != nil {
		t.Fatal(err)
	}
	f.write(f.home, "Documents/notes.md", "milk")
	f.contribute()

	for _, path := range []string{"second/Photos/private.md", "home/../../etc/passwd", "home/secret.pdf", "home/Documents", "home"} {
		result := f.execute("nas_send_file", fmt.Sprintf(`{"path":%q}`, path))
		if !result.IsError {
			t.Fatalf("sending %q was allowed: %+v", path, result)
		}
	}
	if sent := f.outbox.sent(); len(sent) != 0 {
		t.Fatalf("a refused send still delivered something: %+v", sent)
	}
}

// Nothing empty and nothing over the limit leaves the NAS, and each refusal says why so the model
// can tell the user rather than retrying.
func TestSendingRefusesEmptyAndOversizeFiles(t *testing.T) {
	f := newFixture(t)
	f.write(f.home, "Documents/empty.pdf", "")
	if err := os.WriteFile(filepath.Join(f.home, "huge.bin"), make([]byte, (20<<20)+1), 0o644); err != nil {
		t.Fatal(err)
	}
	f.contribute()

	empty := f.execute("nas_send_file", `{"path":"home/Documents/empty.pdf"}`)
	if !empty.IsError || !strings.Contains(empty.Text, "is empty") {
		t.Fatalf("empty file = %+v", empty)
	}
	oversize := f.execute("nas_send_file", `{"path":"home/huge.bin"}`)
	if !oversize.IsError || !strings.Contains(oversize.Text, "only files up to 20.0 MB can be sent") {
		t.Fatalf("oversize file = %+v", oversize)
	}
	if sent := f.outbox.sent(); len(sent) != 0 {
		t.Fatalf("a refused send still delivered something: %+v", sent)
	}
}

// A file that cannot be delivered must not leave the model believing the user has it.
func TestAnUndeliverableFileIsReportedAsNotArrived(t *testing.T) {
	f := newFixture(t)
	f.write(f.home, "Documents/report.pdf", "report")
	f.outbox.err = errors.New("Telegram sendDocument returned HTTP status 500")
	f.contribute()

	result := f.execute("nas_send_file", `{"path":"home/Documents/report.pdf"}`)
	if !result.IsError || !strings.Contains(result.Text, "did not arrive") {
		t.Fatalf("undeliverable file = %+v", result)
	}
}

// The tool is only as available as the deployment that can deliver it, and the guidance carries
// the household's rule: a file is opened only when its content was asked for.
func TestSendingIsAbsentWithoutADelivererAndReadingStaysGated(t *testing.T) {
	f := newFixture(t)
	contribution := f.contribute()
	if !slices.Contains(names(contribution), "nas_send_file") {
		t.Fatalf("tools = %+v", names(contribution))
	}
	if !strings.Contains(contribution.Guidance, "Sending never needs a read") ||
		!strings.Contains(contribution.Guidance, "files stay closed unless they ask for the content") {
		t.Fatalf("guidance = %q", contribution.Guidance)
	}

	f.write(f.home, "Documents/notes.md", "milk")
	withoutDelivery, err := nas.NewService(nas.Config{Roots: []nas.PrincipalRoots{
		{PrincipalID: "alice", Roots: []nas.RootPath{{Name: "home", Path: f.home}}},
	}}, f.store, f.proposals, f.files, nil, slog.New(slog.NewTextHandler(f.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	offered, err := withoutDelivery.Contribute(f.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names(offered), "nas_send_file") {
		t.Fatalf("a deployment that cannot deliver still offered the tool: %+v", names(offered))
	}
	// The file exists, so only the missing sender can refuse this.
	result, err := withoutDelivery.Execute(f.ctx, "alice", tools.Call{Name: "nas_send_file", Arguments: json.RawMessage(`{"path":"home/Documents/notes.md"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Text, "cannot send files") {
		t.Fatalf("an ungranted send succeeded: %+v", result)
	}
}
