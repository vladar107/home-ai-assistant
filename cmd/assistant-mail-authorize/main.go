// Command assistant-mail-authorize authorizes the assistant's own Gmail account with its separate
// Desktop app OAuth client, on the operator's Mac, and prints the refresh token once.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
)

const usageText = `usage: assistant-mail-authorize

Authorizes the assistant's own Gmail account for gmail.readonly and gmail.send through a browser
consent, and prints the refresh token on stdout. It reads the same variables as the application:

  HOME_AGENT_ASSISTANT_MAIL_ADDRESS                  the account to authorize
  HOME_AGENT_ASSISTANT_MAIL_CLIENT_ID                the separate Desktop app OAuth client
  HOME_AGENT_ASSISTANT_MAIL_CLIENT_SECRET(_FILE)     its secret, or a file holding it

For example, with the client ID and address in the environment:

  HOME_AGENT_ASSISTANT_MAIL_CLIENT_SECRET="$(security find-generic-password -a home-ai-assistant -s home-agent.assistant-mail-client-secret -w)" go run ./cmd/assistant-mail-authorize
`

const defaultTimeout = 5 * time.Minute

var scopes = []string{google.MailScope, google.SendScope}

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	client, err := config.LoadAssistantMailClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "assistant-mail-authorize: %v\n\n%s", err, usageText)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, options{Client: client}, os.Stdout, os.Stderr, openBrowser); err != nil {
		fmt.Fprintln(os.Stderr, "assistant-mail-authorize:", err)
		os.Exit(1)
	}
}

func openBrowser(link string) {
	cmd := exec.Command("open", link)
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}

// options are the client to authorize with and, for tests, Google's endpoints and how long to
// wait for the consent.
type options struct {
	Client    config.AssistantMailClient
	AuthURL   string
	TokenURL  string
	RevokeURL string
	GmailURL  string
	Timeout   time.Duration
}

func run(ctx context.Context, opts options, stdout, stderr io.Writer, open func(link string)) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for the consent redirect: %w", err)
	}
	redirectURL := "http://" + listener.Addr().String() + "/"
	oauth, err := google.NewClient(google.ClientConfig{
		ClientID:     opts.Client.ClientID,
		ClientSecret: opts.Client.ClientSecret,
		RedirectURL:  redirectURL,
		AuthURL:      opts.AuthURL,
		TokenURL:     opts.TokenURL,
		RevokeURL:    opts.RevokeURL,
		Scopes:       scopes,
	})
	if err != nil {
		listener.Close()
		return err
	}
	mail, err := gmail.NewClient(gmail.ClientConfig{APIBaseURL: opts.GmailURL})
	if err != nil {
		listener.Close()
		return err
	}
	state, err := randomString(16)
	if err != nil {
		listener.Close()
		return err
	}
	verifier, err := randomString(48)
	if err != nil {
		listener.Close()
		return err
	}
	challenge := sha256.Sum256([]byte(verifier))

	callback := &callback{state: state, result: make(chan redirect, 1)}
	server := &http.Server{Handler: callback, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	// Shutdown, unlike Close, lets the browser receive the result page of the accepted redirect.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	link := oauth.AuthorizationURL(state, base64.RawURLEncoding.EncodeToString(challenge[:]))
	fmt.Fprintf(stderr, "Sign in as %s and allow reading and sending mail:\n\n%s\n\n", opts.Client.Address, link)
	open(link)

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var result redirect
	select {
	case result = <-callback.result:
	case <-waitCtx.Done():
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("no consent arrived within %s", timeout)
		}
		return waitCtx.Err()
	}
	if result.denied != "" {
		return fmt.Errorf("Google returned %q instead of a grant", result.denied)
	}

	tokens, err := oauth.Exchange(ctx, result.code, verifier)
	if err != nil {
		return fmt.Errorf("exchange the authorization code: %w", err)
	}
	revoke := func(reason error) error {
		token := tokens.Refresh
		if token == "" {
			token = tokens.Access
		}
		if err := oauth.Revoke(context.WithoutCancel(ctx), token); err != nil {
			return fmt.Errorf("%w; revoking the grant also failed (%v), so remove the app's access at myaccount.google.com", reason, err)
		}
		return fmt.Errorf("%w; the grant was revoked", reason)
	}
	for _, scope := range scopes {
		if !slices.Contains(tokens.Scopes, scope) {
			return revoke(fmt.Errorf("the consent did not grant %s; run again and leave both permissions ticked", scope))
		}
	}
	profile, err := mail.Profile(ctx, tokens.Access)
	if err != nil {
		return revoke(fmt.Errorf("read the mailbox's address: %w", err))
	}
	if !strings.EqualFold(profile.EmailAddress, opts.Client.Address) {
		return revoke(fmt.Errorf("the consent was given by another account than %s; run again and choose that account", opts.Client.Address))
	}
	if tokens.Refresh == "" {
		return revoke(errors.New("Google issued no refresh token; remove the app's access at myaccount.google.com if it is still listed, and run again"))
	}

	fmt.Fprintln(stdout, tokens.Refresh)
	fmt.Fprintln(stderr, "\nAuthorized. Store the token with scripts/keychain-set-secret home-agent.assistant-mail-refresh-token, then run scripts/nas-sync-secrets.")
	return nil
}

type redirect struct {
	code   string
	denied string
}

// callback accepts the one consent redirect that carries the flow's state; a redirect with any
// other state is refused, and the flow keeps waiting.
type callback struct {
	state  string
	result chan redirect

	mu   sync.Mutex
	done bool
}

func (c *callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	query := r.URL.Query()
	if query.Get("state") != c.state {
		page(w, http.StatusBadRequest, "This sign-in link does not belong to the running authorization.")
		return
	}
	received := redirect{code: query.Get("code"), denied: query.Get("error")}
	if received.code == "" && received.denied == "" {
		page(w, http.StatusBadRequest, "Google sent no authorization code.")
		return
	}
	c.mu.Lock()
	done := c.done
	c.done = true
	c.mu.Unlock()
	if done {
		page(w, http.StatusConflict, "This authorization has already received its answer.")
		return
	}
	c.result <- received
	page(w, http.StatusOK, "Google answered. Return to the terminal; you can close this tab.")
}

func page(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><title>Assistant mailbox</title><p>%s</p>\n", html.EscapeString(text))
}

func randomString(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate the authorization secrets: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
