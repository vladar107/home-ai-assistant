package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
)

const assistantAddress = "assistant@example.com"

type outcome struct {
	err            error
	stdout, stderr string
}

func authorize(t *testing.T, sim *googlesim.Simulator, timeout time.Duration, browse func(link string)) outcome {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), options{
		Client: config.AssistantMailClient{
			Address:      assistantAddress,
			ClientID:     googlesim.AssistantClientID,
			ClientSecret: googlesim.AssistantClientSecret,
		},
		AuthURL:   sim.AuthURL(),
		TokenURL:  sim.TokenURL(),
		RevokeURL: sim.RevokeURL(),
		GmailURL:  sim.URL(),
		Timeout:   timeout,
	}, &stdout, &stderr, browse)
	return outcome{err: err, stdout: stdout.String(), stderr: stderr.String()}
}

// consent plays the operator's browser: the consent screen answers as the account with the grant,
// and the browser follows the redirect to the command's loopback listener.
func consent(t *testing.T, sim *googlesim.Simulator, account string, granted googlesim.Grant) func(string) {
	return func(link string) {
		follow(t, sim.Consent(link, account, granted), http.StatusOK)
	}
}

func follow(t *testing.T, target string, want int) {
	t.Helper()
	response, err := http.Get(target)
	if err != nil {
		t.Fatalf("follow the redirect: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("the redirect answered %d, want %d", response.StatusCode, want)
	}
}

func TestAuthorizePrintsOnlyTheRefreshToken(t *testing.T) {
	sim := googlesim.New(t)

	got := authorize(t, sim, time.Minute, consent(t, sim, assistantAddress, googlesim.Grant{Mail: true, Send: true}))

	if got.err != nil {
		t.Fatalf("run: %v\n%s", got.err, got.stderr)
	}
	token := strings.TrimSuffix(got.stdout, "\n")
	if !strings.HasPrefix(token, "refresh-") || strings.ContainsAny(token, " \n") {
		t.Fatalf("stdout = %q, want the refresh token alone on one line", got.stdout)
	}
	if strings.Contains(got.stderr, token) || strings.Contains(got.stderr, "access-") || strings.Contains(got.stderr, "4/code-") || strings.Contains(got.stderr, googlesim.AssistantClientSecret) {
		t.Fatalf("stderr carries a credential:\n%s", got.stderr)
	}
	if !strings.Contains(got.stderr, "scripts/keychain-set-secret home-agent.assistant-mail-refresh-token") {
		t.Fatalf("stderr has no next step:\n%s", got.stderr)
	}
	if revoked := sim.Revoked(); len(revoked) != 0 {
		t.Fatalf("revoked = %v, want nothing", revoked)
	}
}

func TestAuthorizeRevokesAnIncompleteOrForeignGrant(t *testing.T) {
	for name, tc := range map[string]struct {
		account string
		granted googlesim.Grant
		message string
	}{
		"send not granted": {assistantAddress, googlesim.Grant{Mail: true}, "gmail.send"},
		"another account":  {"someone@example.com", googlesim.Grant{Mail: true, Send: true}, "another account"},
	} {
		t.Run(name, func(t *testing.T) {
			sim := googlesim.New(t)

			got := authorize(t, sim, time.Minute, consent(t, sim, tc.account, tc.granted))

			if got.err == nil || !strings.Contains(got.err.Error(), tc.message) || !strings.Contains(got.err.Error(), "revoked") {
				t.Fatalf("run error = %v, want one naming %q and the revocation", got.err, tc.message)
			}
			if got.stdout != "" {
				t.Fatalf("stdout = %q, want nothing", got.stdout)
			}
			if revoked := sim.Revoked(); !slices.Equal(revoked, []string{tc.account}) {
				t.Fatalf("revoked = %v, want %s", revoked, tc.account)
			}
		})
	}
}

func TestAuthorizeRefusesAnotherStateAndKeepsWaiting(t *testing.T) {
	sim := googlesim.New(t)

	got := authorize(t, sim, time.Minute, func(link string) {
		redirect, err := url.Parse(sim.Consent(link, assistantAddress, googlesim.Grant{Mail: true, Send: true}))
		if err != nil {
			t.Fatal(err)
		}
		forged := *redirect
		query := forged.Query()
		query.Set("state", "another-state")
		forged.RawQuery = query.Encode()
		follow(t, forged.String(), http.StatusBadRequest)
		follow(t, redirect.String(), http.StatusOK)
		follow(t, redirect.String(), http.StatusConflict)
	})

	if got.err != nil || !strings.HasPrefix(got.stdout, "refresh-") {
		t.Fatalf("run = %v, stdout %q\n%s", got.err, got.stdout, got.stderr)
	}
}

func TestAuthorizeReportsADeniedConsent(t *testing.T) {
	sim := googlesim.New(t)
	page := make(chan string, 1)

	got := authorize(t, sim, time.Minute, func(link string) {
		parsed, err := url.Parse(link)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		denied := query.Get("redirect_uri") + "?" + url.Values{"state": {query.Get("state")}, "error": {"access_denied"}}.Encode()
		// The browser follows the redirect while the command goes on, as a real one does, so the
		// page must survive the command stopping its listener.
		go func() {
			response, err := http.Get(denied)
			if err != nil {
				page <- "request failed: " + err.Error()
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				page <- "read failed: " + err.Error()
				return
			}
			page <- string(body)
		}()
	})

	if got.err == nil || !strings.Contains(got.err.Error(), "access_denied") || got.stdout != "" {
		t.Fatalf("run = %v, stdout %q", got.err, got.stdout)
	}
	if body := <-page; !strings.Contains(body, "Return to the terminal") {
		t.Fatalf("the browser received %q, want the result page", body)
	}
	for _, request := range sim.Requests() {
		if request.Path == "/revoke" || request.Path == "/token" {
			t.Fatalf("a denied consent reached Google's %s", request.Path)
		}
	}
}

func TestAuthorizeGivesUpWithoutConsent(t *testing.T) {
	sim := googlesim.New(t)

	got := authorize(t, sim, 50*time.Millisecond, func(string) {})

	if got.err == nil || !strings.Contains(got.err.Error(), "no consent arrived") || got.stdout != "" {
		t.Fatalf("run = %v, stdout %q", got.err, got.stdout)
	}
}
