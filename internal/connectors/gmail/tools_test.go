package gmail

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	work = "ann@work.example"
	home = "ann@home.example"
)

type staticGrants struct {
	accounts []google.Account
	tokens   map[string]string
}

func (g *staticGrants) Accounts(context.Context, string, google.Product) ([]google.Account, error) {
	return g.accounts, nil
}

func (g *staticGrants) AccessToken(_ context.Context, _, accountID string, _ google.Product, _ bool) (string, error) {
	return g.tokens[accountID], nil
}

type harness struct {
	t       *testing.T
	sim     *googlesim.Simulator
	grants  *staticGrants
	service *Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	sim := googlesim.New(t)
	grants := &staticGrants{tokens: map[string]string{}}
	service, err := NewService(Config{APIBaseURL: sim.URL(), HTTPClient: sim.Client()}, grants, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, sim: sim, grants: grants, service: service}
}

// connect signs the account in at the simulator with the products the user left ticked, the
// way the Google module does, and hands its access token to the Gmail service.
func (h *harness) connect(address string, granted googlesim.Grant) {
	h.t.Helper()
	client, err := google.NewClient(google.ClientConfig{
		ClientID: googlesim.ClientID, ClientSecret: googlesim.ClientSecret, RedirectURL: googlesim.RedirectURL,
		AuthURL: h.sim.AuthURL(), TokenURL: h.sim.TokenURL(), RevokeURL: h.sim.RevokeURL(), UserInfoURL: h.sim.UserInfoURL(),
		HTTPClient: h.sim.Client(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	verifier := "verifier-for-" + address
	challenge := sha256.Sum256([]byte(verifier))
	redirect, err := url.Parse(h.sim.Consent(client.AuthorizationURL("state", base64.RawURLEncoding.EncodeToString(challenge[:])), address, granted))
	if err != nil {
		h.t.Fatal(err)
	}
	tokens, err := client.Exchange(context.Background(), redirect.Query().Get("code"), verifier)
	if err != nil {
		h.t.Fatal(err)
	}
	h.grants.accounts = append(h.grants.accounts, google.Account{ID: "id-" + address, Address: address})
	h.grants.tokens["id-"+address] = tokens.Access
}

func (h *harness) search(arguments map[string]any) tools.Result {
	h.t.Helper()
	return h.execute(toolSearch, arguments)
}

func (h *harness) read(account, id string) tools.Result {
	h.t.Helper()
	return h.execute(toolRead, map[string]any{"account": account, "id": id})
}

func (h *harness) execute(name string, arguments map[string]any) tools.Result {
	h.t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		h.t.Fatal(err)
	}
	result, err := h.service.Execute(context.Background(), "principal-1", tools.Call{Name: name, Arguments: raw})
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

func invoices(prefix string, count int) []googlesim.Message {
	messages := make([]googlesim.Message, count)
	for index := range messages {
		id := prefix + string(rune('a'+index))
		messages[index] = googlesim.Message{
			ID: id, ThreadID: id, From: "billing@shop.example", Subject: "Invoice " + id,
			Received: time.Date(2026, 10, 1+index, 9, 0, 0, 0, time.UTC),
		}
	}
	return messages
}

func requireText(t *testing.T, result tools.Result, wantError bool, fragments ...string) {
	t.Helper()
	if result.IsError != wantError {
		t.Fatalf("IsError = %v, want %v:\n%s", result.IsError, wantError, result.Text)
	}
	for _, fragment := range fragments {
		if !strings.Contains(result.Text, fragment) {
			t.Fatalf("result lacks %q:\n%s", fragment, result.Text)
		}
	}
}

func TestAMessageThatCannotBeReadIsSkippedAndCounted(t *testing.T) {
	h := newHarness(t)
	h.connect(work, googlesim.Grant{Mail: true})
	h.sim.AddMailbox(work, invoices("w", 3)...)
	h.sim.FailMessage("wb", http.StatusForbidden)

	result := h.search(map[string]any{"query": "invoice"})
	requireText(t, result, false, "2 messages match", "id wa", "id wc", "1 of the matching messages in "+work+" could not be read: "+forbiddenText)
	if strings.Contains(result.Text, "id wb") {
		t.Fatalf("the unreadable message was listed:\n%s", result.Text)
	}
}

func TestARateLimitStopsReadingTheMailboxButKeepsTheOthers(t *testing.T) {
	h := newHarness(t)
	h.connect(work, googlesim.Grant{Mail: true})
	h.connect(home, googlesim.Grant{Mail: true})
	h.sim.AddMailbox(work, invoices("w", 3)...)
	h.sim.AddMailbox(home, invoices("h", 1)...)
	h.sim.FailMessage("wa", http.StatusTooManyRequests)

	result := h.search(map[string]any{"query": "invoice"})
	requireText(t, result, false, "id ha", work+" could not be searched: Gmail is limiting requests right now")
	for _, request := range h.sim.Requests() {
		if strings.HasSuffix(request.Path, "/messages/wb") || strings.HasSuffix(request.Path, "/messages/wc") {
			t.Fatalf("messages after the rate limit were still read: %+v", request)
		}
	}
}

func TestARefusedMailboxSaysSoInsteadOfUnavailable(t *testing.T) {
	h := newHarness(t)
	h.connect(work, googlesim.Grant{Calendar: true})
	h.sim.AddMailbox(work, invoices("w", 1)...)

	requireText(t, h.search(map[string]any{"query": "invoice"}), true, forbiddenText)
}

// A message is read as its HTML shows it, the way a mail client shows it, even when the sender's
// plain-text alternative says something else.
func TestAMessageIsReadAsItsHTMLShowsIt(t *testing.T) {
	h := newHarness(t)
	h.connect(work, googlesim.Grant{Mail: true})
	received := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	booking := `<html><head><title>Head title</title><style>td{}</style></head><body>
<div style="display:none!important;max-height:0">Your trip is booked</div>
<span style="opacity:0;max-height:0;overflow:hidden">Preview&zwnj;&nbsp;&zwnj;&nbsp;&#847;&shy;&#8203;&#65279;</span>
<table><tr><td>Date of birth</td><td>12 May 1980</td></tr><tr><td>Seat</td><td>14C</td></tr></table>
<p style="Visibility: Hidden !important">Hidden note</p>
<div style="overflow:hidden; max-height:0">Shown despite overflow</div>
<div style="display:none; display:block">Shown by the last declaration</div>
<p><a href="https://track.example/c/abc?u=1">Manage your booking</a> or <a href="cid:logo"><img src="cid:logo" alt="Airline"></a></p>
</body></html>`
	h.sim.AddMailbox(work,
		googlesim.Message{ID: "both", ThreadID: "both", From: "booking@airline.example", Subject: "Booking", Received: received,
			PlainText: "Date of birth 01-01-1970", HTML: booking},
		googlesim.Message{ID: "empty", ThreadID: "empty", From: "news@shop.example", Subject: "News", Received: received,
			PlainText: "Plain only text", HTML: `<html><body><div style="display:none">x</div><a href="https://shop.example/"></a></body></html>`},
		googlesim.Message{ID: "image", ThreadID: "image", From: "news@shop.example", Subject: "Sale", Received: received,
			PlainText: "Plain sale text", HTML: `<img src="https://shop.example/sale.png" alt="Sale until Sunday">`},
		googlesim.Message{ID: "latin", ThreadID: "latin", From: "cafe@example.com", Subject: "Menu", Received: received,
			HTML: "<p>Caf\xe9 cr\xe8me</p>", Charset: "windows-1252"},
		googlesim.Message{ID: "ascii", ThreadID: "ascii", From: "cafe@example.com", Subject: "Menu", Received: received,
			HTML: "<p>Café crème</p>", Charset: "us-ascii"},
		googlesim.Message{ID: "plain", ThreadID: "plain", From: "friend@example.com", Subject: "Hi", Received: received,
			PlainText: "Just plain text.\n\nSecond paragraph."},
	)

	read := h.read(work, "both")
	requireText(t, read, false, "Preview\n", "Date of birth 12 May 1980\nSeat 14C", "Shown despite overflow", "Shown by the last declaration",
		"Manage your booking or [image: Airline]")
	for _, absent := range []string{"1970", "Your trip is booked", "Hidden note", "track.example", "cid:", "Head title", "td{}", "\u200c", "\u034f", "\u00ad", "\u200b", "\ufeff"} {
		if strings.Contains(read.Text, absent) {
			t.Fatalf("read message contains %q:\n%s", absent, read.Text)
		}
	}

	requireText(t, h.read(work, "empty"), false, "\n\nPlain only text")
	if image := h.read(work, "image"); !strings.Contains(image.Text, "[image: Sale until Sunday]") || strings.Contains(image.Text, "Plain sale text") {
		t.Fatalf("image-only HTML = %+v", image)
	}
	requireText(t, h.read(work, "latin"), false, "Café crème")
	requireText(t, h.read(work, "ascii"), false, "Café crème")
	requireText(t, h.read(work, "plain"), false, "\n\nJust plain text.\n\nSecond paragraph.")
}
