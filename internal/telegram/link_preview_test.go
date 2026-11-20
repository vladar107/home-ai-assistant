package telegram

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/events"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

const disabledLinkPreview = `{"is_disabled":true}`

type fixedTripChat int64

func (c fixedTripChat) ResolveActiveChat(context.Context, string) (int64, bool, error) {
	return int64(c), true, nil
}

// previewServer fails the test for any sendMessage or editMessageText that does not disable link
// previews, and answers the first request with the unparsable-entities error when refuseFirst.
func previewServer(t *testing.T, refuseFirst bool) (*Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var methods []string
	refused := !refuseFirst
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("form: %v", err)
		}
		method := path.Base(r.URL.Path)
		if method == "sendMessage" || method == "editMessageText" {
			if got := r.Form.Get("link_preview_options"); got != disabledLinkPreview {
				t.Errorf("%s link_preview_options = %q, want %s", method, got, disabledLinkPreview)
			}
		}
		mu.Lock()
		methods = append(methods, method)
		first := !refused
		refused = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: Unsupported start tag"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":7}}`)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{BaseURL: server.URL, Token: "test-token", PollTimeout: time.Second, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), methods...)
	}
}

func TestSendsDisableLinkPreviews(t *testing.T) {
	t.Parallel()
	const chatID = 101
	t.Run("chat reply", func(t *testing.T) {
		t.Parallel()
		client, sent := previewServer(t, false)
		if _, _, err := client.SendTrackedReply(context.Background(), chatID, 5, "[label](https://example.com/?q=secret)"); err != nil {
			t.Fatal(err)
		}
		if len(sent()) == 0 {
			t.Fatal("nothing was sent")
		}
	})
	t.Run("edit", func(t *testing.T) {
		t.Parallel()
		client, sent := previewServer(t, false)
		if err := client.EditKeyboard(context.Background(), chatID, 7, "https://example.com/", InlineKeyboard{}); err != nil {
			t.Fatal(err)
		}
		if got := sent(); len(got) != 1 || got[0] != "editMessageText" {
			t.Fatalf("methods = %v, want one editMessageText", got)
		}
	})
	t.Run("event", func(t *testing.T) {
		t.Parallel()
		client, sent := previewServer(t, false)
		message := events.Message{Source: "Example", Title: "Title", Text: "https://example.com/"}
		if _, err := (EventSender{Client: client}).SendEvent(context.Background(), chatID, message); err != nil {
			t.Fatal(err)
		}
		if got := sent(); len(got) != 1 {
			t.Fatalf("methods = %v, want one send", got)
		}
	})
	t.Run("event plain fallback", func(t *testing.T) {
		t.Parallel()
		client, sent := previewServer(t, true)
		message := events.Message{Source: "Example", Title: "Title", Text: "https://example.com/"}
		if _, err := (EventSender{Client: client}).SendEvent(context.Background(), chatID, message); err != nil {
			t.Fatal(err)
		}
		if got := sent(); len(got) != 2 {
			t.Fatalf("methods = %v, want the refused send and its plain retry", got)
		}
	})
	t.Run("trip card", func(t *testing.T) {
		t.Parallel()
		client, sent := previewServer(t, false)
		found := trip.Trip{ID: "trip-1", PrincipalID: "alice", Title: "Example trip"}
		if err := (TripNotifier{Client: client, Chats: fixedTripChat(chatID)}).SendTripCard(context.Background(), found); err != nil {
			t.Fatal(err)
		}
		if len(sent()) == 0 {
			t.Fatal("nothing was sent")
		}
	})
}
