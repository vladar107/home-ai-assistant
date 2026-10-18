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
	raw, err := json.Marshal(arguments)
	if err != nil {
		h.t.Fatal(err)
	}
	result, err := h.service.Execute(context.Background(), "principal-1", tools.Call{Name: toolSearch, Arguments: raw})
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
