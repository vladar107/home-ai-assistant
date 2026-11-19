package googlesim

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAssistantConsentGrantsSendOnlyWhenTicked(t *testing.T) {
	for name, granted := range map[string]Grant{
		"send ticked":   {Mail: true, Send: true},
		"send unticked": {Mail: true},
	} {
		t.Run(name, func(t *testing.T) {
			sim := New(t)
			verifier := "assistant-verifier"
			challenge := sha256.Sum256([]byte(verifier))
			redirectURL := "http://127.0.0.1:53682/"
			link := sim.AuthURL() + "?" + url.Values{
				"client_id": {AssistantClientID}, "redirect_uri": {redirectURL}, "response_type": {"code"},
				"scope": {sendScope + " " + mailScope}, "access_type": {"offline"}, "state": {"state-1"},
				"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
			}.Encode()

			redirect, err := url.Parse(sim.Consent(link, "assistant@example.com", granted))
			if err != nil {
				t.Fatal(err)
			}
			query := redirect.Query()
			if redirect.Host != "127.0.0.1:53682" || query.Get("state") != "state-1" || hasScope(query.Get("scope"), sendScope) != granted.Send {
				t.Fatalf("redirect = %s", redirect)
			}

			tokens := exchange(t, sim, url.Values{
				"grant_type": {"authorization_code"}, "code": {query.Get("code")}, "code_verifier": {verifier}, "redirect_uri": {redirectURL},
				"client_id": {AssistantClientID}, "client_secret": {AssistantClientSecret},
			})
			if tokens["refresh_token"] == nil || hasScope(tokens["scope"].(string), sendScope) != granted.Send || !hasScope(tokens["scope"].(string), mailScope) {
				t.Fatalf("tokens = %v", tokens)
			}

			household := exchange(t, sim, url.Values{
				"grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)},
				"client_id": {ClientID}, "client_secret": {ClientSecret},
			})
			if household["error"] != "invalid_grant" {
				t.Fatalf("the household client refreshed the assistant's token: %v", household)
			}

			request, err := http.NewRequest(http.MethodPost, sim.URL()+"/gmail/v1/users/me/messages/send",
				strings.NewReader(`{"raw":"`+base64.URLEncoding.EncodeToString([]byte("To: shop@example.org\r\nSubject: Hello\r\n\r\nHello.\r\n"))+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+tokens["access_token"].(string))
			response, err := sim.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if want := map[bool]int{true: http.StatusOK, false: http.StatusForbidden}[granted.Send]; response.StatusCode != want {
				t.Fatalf("send answered %d, want %d", response.StatusCode, want)
			}
			for _, recorded := range sim.Requests() {
				if strings.Contains(recorded.Query.Encode(), "refresh-") || strings.Contains(recorded.Query.Encode(), "access-") {
					t.Fatalf("a token was recorded: %+v", recorded)
				}
			}
		})
	}
}

func exchange(t *testing.T, sim *Simulator, form url.Values) map[string]any {
	t.Helper()
	response, err := sim.Client().PostForm(sim.TokenURL(), form)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}
