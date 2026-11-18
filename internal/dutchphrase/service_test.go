package dutchphrase_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/delivery"
	"github.com/vladar107/home-ai-assistant/internal/dutchphrase"
	"github.com/vladar107/home-ai-assistant/internal/export"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
)

var now = time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)

type call struct {
	principalID, message string
	language             locale.Language
}

// checker stands in for the model turn: it answers each call with the next reply.
type checker struct {
	mu      sync.Mutex
	replies []string
	kind    string
	calls   []call
}

func (c *checker) RunPhrase(ctx context.Context, principalID, message string) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call{principalID: principalID, message: message, language: locale.From(ctx)})
	if c.kind != "" {
		return "The run failed.", c.kind, nil
	}
	if len(c.replies) == 0 {
		return "", "", nil
	}
	reply := c.replies[0]
	c.replies = c.replies[1:]
	return reply, "", nil
}

type candidate struct {
	Phrase           string `json:"phrase"`
	Usage            string `json:"usage"`
	Translation      string `json:"translation"`
	UsageTranslation string `json:"usage_translation"`
	Note             string `json:"note,omitempty"`
}

func reply(t *testing.T, candidates ...candidate) string {
	t.Helper()
	data, err := json.Marshal(map[string][]candidate{"candidates": candidates})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var (
	receipt = candidate{Phrase: "Mag ik de bon?", Usage: "Je vraagt dit bij de kassa.", Translation: "May I have the receipt?", UsageTranslation: "You ask this at the till.", Note: "bon: a receipt, not a voucher"}
	rain    = candidate{Phrase: "Het regent pijpenstelen.", Usage: "Je zegt dit als het hard regent.", Translation: "It is raining cats and dogs.", UsageTranslation: "You say this when it rains hard."}
	turn    = candidate{Phrase: "Jij bent aan de beurt.", Usage: "Je zegt dit in een rij.", Translation: "It is your turn.", UsageTranslation: "You say this in a queue."}
)

const (
	rainBlock    = "🇳🇱 **Dutch phrase of the day**\n**Het regent pijpenstelen.**\nJe zegt dit als het hard regent.\n||It is raining cats and dogs. — You say this when it rains hard.||"
	receiptBlock = "🇳🇱 **Dutch phrase of the day**\n**Mag ik de bon?**\nJe vraagt dit bij de kassa.\n||May I have the receipt? — You ask this at the till.||\n||bon: a receipt, not a voucher||"
)

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func newService(t *testing.T, store *sqlite.Store, c *checker, logs io.Writer) *dutchphrase.Service {
	t.Helper()
	if logs == nil {
		logs = io.Discard
	}
	service, err := dutchphrase.NewService(dutchphrase.Config{
		Repository: store, Checker: c, Preferences: store,
		Logger: slog.New(slog.NewTextHandler(logs, nil)), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func amsterdam(t *testing.T) *time.Location {
	t.Helper()
	zone, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	return zone
}

func phrase(t *testing.T, service *dutchphrase.Service, principalID string, scheduledFor time.Time, manual bool) string {
	t.Helper()
	block, err := service.Phrase(t.Context(), principalID, scheduledFor, amsterdam(t), "", manual)
	if err != nil {
		t.Fatal(err)
	}
	return block
}

func TestNewServiceNeedsItsParts(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	for name, cfg := range map[string]dutchphrase.Config{
		"repository":  {Checker: &checker{}, Preferences: store},
		"checker":     {Repository: store, Preferences: store},
		"preferences": {Repository: store, Checker: &checker{}},
	} {
		if _, err := dutchphrase.NewService(cfg); err == nil {
			t.Errorf("a service without %s was made", name)
		}
	}
}

// The day's phrase is the first candidate never sent to the user, compared without case,
// punctuation, or spacing, and it belongs to the local date of the run.
func TestPhraseTakesTheFirstCandidateNeverSent(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	again := receipt
	again.Phrase = "mag ik  de BON!"
	var logs bytes.Buffer
	c := &checker{replies: []string{reply(t, receipt), reply(t, again, rain)}}
	service := newService(t, store, c, &logs)

	if got := phrase(t, service, "alice", now, false); got != receiptBlock {
		t.Fatalf("first day:\n%s\nwant:\n%s", got, receiptBlock)
	}
	// 23:30 UTC is already the next day in Amsterdam.
	late := time.Date(2026, 11, 19, 23, 30, 0, 0, time.UTC)
	if got := phrase(t, service, "alice", late, false); got != rainBlock {
		t.Fatalf("second day:\n%s\nwant:\n%s", got, rainBlock)
	}
	stored, found, err := store.LoadDutchPhrase(t.Context(), "alice", "2026-11-20")
	if err != nil || !found || stored.Phrase != rain.Phrase || stored.Normalized != "het regent pijpenstelen" || stored.Note != "" || !stored.CreatedAt.Equal(now) {
		t.Fatalf("stored = %+v %v %v", stored, found, err)
	}
	if len(c.calls) != 2 || c.calls[0].principalID != "alice" || c.calls[1].principalID != "alice" {
		t.Fatalf("calls = %+v, want two as alice", c.calls)
	}
	if !strings.Contains(c.calls[1].message, "> Mag ik de bon?") {
		t.Fatalf("the second prompt does not list the phrase sent before:\n%s", c.calls[1].message)
	}
	if strings.Contains(logs.String(), "pijpenstelen") || strings.Contains(logs.String(), "bon") {
		t.Fatalf("the logs hold phrase text:\n%s", logs.String())
	}
}

// A phrase sent to one user is no reason to hold it back from another.
func TestPhraseHistoryIsEachUsersOwn(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	c := &checker{replies: []string{reply(t, rain), reply(t, rain)}}
	service := newService(t, store, c, nil)
	if got := phrase(t, service, "alice", now, false); got != rainBlock {
		t.Fatalf("alice got %q", got)
	}
	if got := phrase(t, service, "bob", now, false); got != rainBlock {
		t.Fatalf("bob got %q", got)
	}
	if c.calls[1].principalID != "bob" || strings.Contains(c.calls[1].message, "pijpenstelen") {
		t.Fatalf("bob's turn = %+v", c.calls[1])
	}
}

func TestNoPhraseWhenEveryCandidateWasSentOrTheReplyIsUnusable(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		reply string
		kind  string
	}{
		"all sent":    {reply: `{"candidates":[{"phrase":"Het regent pijpenstelen","usage":"a","translation":"b","usage_translation":"c"},{"phrase":"MAG IK DE BON","usage":"a","translation":"b","usage_translation":"c"}]}`},
		"unreadable":  {reply: "Here is a nice phrase: Mag ik de bon?"},
		"no fields":   {reply: `{"candidates":[{"phrase":"Tot zo!"}]}`},
		"empty":       {reply: `{"candidates":[]}`},
		"turn failed": {kind: "timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := openStore(t)
			c := &checker{replies: []string{reply(t, rain), reply(t, receipt), test.reply}}
			service := newService(t, store, c, nil)
			phrase(t, service, "alice", now.AddDate(0, 0, -2), false)
			phrase(t, service, "alice", now.AddDate(0, 0, -1), false)
			c.kind = test.kind
			if got := phrase(t, service, "alice", now, false); got != "" {
				t.Fatalf("got %q, want no phrase", got)
			}
			if _, found, err := store.LoadDutchPhrase(t.Context(), "alice", "2026-11-18"); err != nil || found {
				t.Fatalf("a row was stored: %v %v", found, err)
			}
			if recent, err := store.RecentDutchPhrases(t.Context(), "alice", 100); err != nil || len(recent) != 2 {
				t.Fatalf("history = %v %v", recent, err)
			}
		})
	}
}

// A candidate whose text could break the block's markup is skipped for the next one.
func TestCandidatesThatBreakTheMarkupAreSkipped(t *testing.T) {
	t.Parallel()
	newline, backtick, bold, spoiler, long, bare := turn, turn, turn, turn, turn, turn
	newline.Usage = "Je zegt dit\nin een rij."
	backtick.Translation = "It is `your` turn."
	bold.Phrase = "Jij bent **aan** de beurt."
	spoiler.Note = "beurt || turn"
	long.Translation = strings.Repeat("a", dutchphrase.MaximumTextRunes+1)
	bare.Note = "Zie https://example.com voor meer."
	store := openStore(t)
	c := &checker{replies: []string{reply(t, newline, backtick, bold, spoiler, rain)}}
	service := newService(t, store, c, nil)
	if got := phrase(t, service, "alice", now, false); got != rainBlock {
		t.Fatalf("got:\n%s\nwant:\n%s", got, rainBlock)
	}
	c.replies = []string{reply(t, long, bare, receipt)}
	if got := phrase(t, service, "alice", now.AddDate(0, 0, 1), false); got != receiptBlock {
		t.Fatalf("got:\n%s\nwant:\n%s", got, receiptBlock)
	}
	link, quote, closing, starred, opening := turn, turn, turn, turn, turn
	link.Usage = "Zie [hier](https://example.com)."
	quote.Usage = "> Je zegt dit in een rij."
	closing.Translation = "It is your turn|"
	starred.Phrase = "Jij bent aan de beurt*"
	opening.UsageTranslation = "*You say this in a queue."
	c.replies = []string{reply(t, link, quote, closing, starred, opening), reply(t, turn)}
	if got := phrase(t, service, "alice", now.AddDate(0, 0, 2), false); got != "" {
		t.Fatalf("a candidate with markup was taken:\n%s", got)
	}
	if got := phrase(t, service, "alice", now.AddDate(0, 0, 2), false); !strings.Contains(got, "**Jij bent aan de beurt.**") {
		t.Fatalf("the clean candidate was not taken:\n%s", got)
	}
}

// Only the run that stores the day's phrase adds it; a manual run shows it again without a turn.
func TestOnePhraseADay(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	c := &checker{replies: []string{reply(t, rain), reply(t, receipt)}}
	service := newService(t, store, c, nil)
	if got := phrase(t, service, "alice", now, false); got != rainBlock {
		t.Fatalf("first run got %q", got)
	}
	if got := phrase(t, service, "alice", now.Add(4*time.Hour), false); got != "" {
		t.Fatalf("a second scheduled run got %q", got)
	}
	if got := phrase(t, service, "alice", now.Add(5*time.Hour), true); got != rainBlock {
		t.Fatalf("a manual run got %q", got)
	}
	if len(c.calls) != 1 {
		t.Fatalf("the turn ran %d times, want once", len(c.calls))
	}
	// A manual run on a day without a phrase makes one.
	if got := phrase(t, service, "alice", now.AddDate(0, 0, 1), true); got != receiptBlock {
		t.Fatalf("a manual run on a new day got %q", got)
	}
}

// The translations and the labels are in the user's interface language, and the notes reach the
// turn.
func TestPhraseIsInTheUsersLanguage(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	if err := store.SaveLanguage(t.Context(), "bob", locale.Russian); err != nil {
		t.Fatal(err)
	}
	ru := candidate{Phrase: "Mag ik de bon?", Usage: "Je vraagt dit bij de kassa.", Translation: "Можно чек?", UsageTranslation: "Так спрашивают на кассе."}
	c := &checker{replies: []string{reply(t, ru)}}
	service := newService(t, store, c, nil)
	block, err := service.Phrase(t.Context(), "bob", now, amsterdam(t), "Use phrases for the office.", false)
	if err != nil {
		t.Fatal(err)
	}
	want := "🇳🇱 **Фраза дня на нидерландском**\n**Mag ik de bon?**\nJe vraagt dit bij de kassa.\n||Можно чек? — Так спрашивают на кассе.||"
	if block != want {
		t.Fatalf("got:\n%s\nwant:\n%s", block, want)
	}
	sent := c.calls[0]
	if sent.principalID != "bob" || sent.language != locale.Russian || !strings.Contains(sent.message, "translation is the phrase in Russian") || !strings.Contains(sent.message, "> Use phrases for the office.") {
		t.Fatalf("turn = %+v", sent)
	}
}

type files struct{}

func (files) SendFile(context.Context, string, delivery.File) error { return nil }

type offers struct{}

func (offers) Offer(context.Context, string, int64, int64, string, time.Duration, []approval.Proposal) ([]approval.Record, error) {
	return nil, nil
}

// The history survives an export and restore, so a restored account repeats no phrase; a phrase
// already there is left alone, and so is one for a day that already has another.
func TestExportRestoresThePhraseHistory(t *testing.T) {
	t.Parallel()
	source, target := openStore(t), openStore(t)
	c := &checker{replies: []string{reply(t, rain), reply(t, receipt), reply(t, rain), reply(t, turn)}}
	phrase(t, newService(t, source, c, nil), "alice", now.AddDate(0, 0, -1), false)
	phrase(t, newService(t, source, c, nil), "alice", now, false)
	phrase(t, newService(t, target, c, nil), "alice", now.AddDate(0, 0, -5), false)
	phrase(t, newService(t, target, c, nil), "alice", now, false)

	keyPath := filepath.Join(t.TempDir(), "export.key")
	build := func(store *sqlite.Store) *export.Service {
		service, err := export.NewService(export.Config{KeyPath: keyPath, Now: func() time.Time { return now }}, store, files{}, offers{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Register(dutchphrase.Exporter()); err != nil {
			t.Fatal(err)
		}
		return service
	}
	file, err := build(source).Export(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	report, err := build(target).Restore(t.Context(), "alice", file.Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sections) != 1 {
		t.Fatalf("report = %+v", report.Sections)
	}
	section := report.Sections[0]
	if section.Failed || len(section.Added) != 0 || section.Present != 1 || len(section.Skipped) != 1 || section.Skipped[0].Reason != "you already have a phrase for that day" {
		t.Fatalf("section = %+v, want the rain present and the receipt skipped for its taken day", section)
	}

	fresh := openStore(t)
	if report, err = build(fresh).Restore(t.Context(), "alice", file.Data); err != nil || report.Additions() != 2 {
		t.Fatalf("restore into an empty account = %+v %v", report, err)
	}
	for date, want := range map[string]candidate{"2026-11-17": rain, "2026-11-18": receipt} {
		got, found, err := fresh.LoadDutchPhrase(t.Context(), "alice", date)
		if err != nil || !found || got.Phrase != want.Phrase || got.Usage != want.Usage || got.Translation != want.Translation ||
			got.UsageTranslation != want.UsageTranslation || got.Note != want.Note || got.Normalized != dutchphrase.Normalize(want.Phrase) {
			t.Fatalf("%s restored as %+v %v %v", date, got, found, err)
		}
	}
	c.replies = []string{reply(t, rain, receipt)}
	if got := phrase(t, newService(t, fresh, c, nil), "alice", now.AddDate(0, 0, 1), false); got != "" {
		t.Fatalf("a restored phrase was sent again: %q", got)
	}
}

// racing stores another run's phrase for the day just before the service stores its own.
type racing struct {
	*sqlite.Store
	rival dutchphrase.Phrase
}

func (r racing) InsertDutchPhrase(ctx context.Context, phrase dutchphrase.Phrase) (bool, error) {
	if _, err := r.Store.InsertDutchPhrase(ctx, r.rival); err != nil {
		return false, err
	}
	return r.Store.InsertDutchPhrase(ctx, phrase)
}

// Of two runs racing for the day's phrase, the one that loses adds nothing when scheduled and
// shows the winner's phrase when the user ran it.
func TestARunThatLosesTheRaceShowsTheStoredPhrase(t *testing.T) {
	t.Parallel()
	for name, manual := range map[string]bool{"scheduled": false, "manual": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := openStore(t)
			rival := dutchphrase.Phrase{
				PrincipalID: "alice", LocalDate: "2026-11-18", Phrase: receipt.Phrase, Normalized: dutchphrase.Normalize(receipt.Phrase),
				Usage: receipt.Usage, Translation: receipt.Translation, UsageTranslation: receipt.UsageTranslation, Note: receipt.Note, CreatedAt: now,
			}
			service, err := dutchphrase.NewService(dutchphrase.Config{
				Repository: racing{Store: store, rival: rival}, Checker: &checker{replies: []string{reply(t, rain)}}, Preferences: store, Now: func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if manual {
				want = receiptBlock
			}
			if got := phrase(t, service, "alice", now, manual); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			if sent, err := store.SentDutchPhrases(t.Context(), "alice", []string{dutchphrase.Normalize(rain.Phrase)}); err != nil || len(sent) != 0 {
				t.Fatalf("the losing phrase was stored: %v %v", sent, err)
			}
		})
	}
}

// tampered stands in for a source store whose export holds rows the file could carry but the table
// never would: a null required field, a number for a note, and a null note.
type tampered struct{ *sqlite.Store }

func (s tampered) ExportRows(ctx context.Context, table, principalID string, omit []string) ([]export.Row, error) {
	rows, err := s.Store.ExportRows(ctx, table, principalID, omit)
	if err != nil || table != "dutch_phrases" {
		return rows, err
	}
	row := func(date, phrase string) export.Row {
		return export.Row{"local_date": date, "phrase": phrase, "normalized": dutchphrase.Normalize(phrase), "usage": "Je zegt dit.",
			"translation": "A translation.", "usage_translation": "You say this.", "note": "", "created_at": "2026-11-01T06:00:00.000Z"}
	}
	nullUsage, numericNote, nullNote := row("2026-11-01", "Tot straks."), row("2026-11-02", "Tot zo."), row("2026-11-03", "Tot morgen.")
	nullUsage["usage"], numericNote["note"], nullNote["note"] = nil, 7, nil
	return append(rows, nullUsage, numericNote, nullNote), nil
}

// A row the restore cannot read is skipped on its own, and the rows around it are still restored.
func TestRestoreSkipsAnUnreadableRow(t *testing.T) {
	t.Parallel()
	source, target := openStore(t), openStore(t)
	phrase(t, newService(t, source, &checker{replies: []string{reply(t, rain)}}, nil), "alice", now, false)
	keyPath := filepath.Join(t.TempDir(), "export.key")
	build := func(store export.Store) *export.Service {
		service, err := export.NewService(export.Config{KeyPath: keyPath, Now: func() time.Time { return now }}, store, files{}, offers{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Register(dutchphrase.Exporter()); err != nil {
			t.Fatal(err)
		}
		return service
	}
	file, err := build(tampered{source}).Export(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	report, err := build(target).Restore(t.Context(), "alice", file.Data)
	if err != nil {
		t.Fatal(err)
	}
	section := report.Sections[0]
	if section.Failed || len(section.Added) != 2 || len(section.Skipped) != 2 {
		t.Fatalf("section = %+v, want two added and two skipped", section)
	}
	for _, skipped := range section.Skipped {
		if skipped.Reason != "it could not be read" {
			t.Fatalf("skipped = %+v", section.Skipped)
		}
	}
	if got, found, err := target.LoadDutchPhrase(t.Context(), "alice", "2026-11-18"); err != nil || !found || got.Phrase != rain.Phrase {
		t.Fatalf("the valid phrase was not restored: %+v %v %v", got, found, err)
	}
	if got, found, err := target.LoadDutchPhrase(t.Context(), "alice", "2026-11-03"); err != nil || !found || got.Note != "" || got.Phrase != "Tot morgen." {
		t.Fatalf("the phrase with a null note was not restored: %+v %v %v", got, found, err)
	}
	for _, date := range []string{"2026-11-01", "2026-11-02"} {
		if _, found, err := target.LoadDutchPhrase(t.Context(), "alice", date); err != nil || found {
			t.Fatalf("the unreadable row of %s was restored: %v %v", date, found, err)
		}
	}
}
