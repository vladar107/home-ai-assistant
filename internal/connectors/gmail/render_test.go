package gmail

import (
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A long message is read in windows that never split a character, and the offset each window
// names continues exactly where it stopped.
func TestLongMessagesAreReadInContinuingWindows(t *testing.T) {
	body := strings.Repeat("Größe ", 5000)
	var message Message
	message.Payload.MimeType = "text/plain"
	message.Payload.Body.Data = base64.URLEncoding.EncodeToString([]byte(body))

	continuation := regexp.MustCompile(`continue with offset_bytes (\d+)\]$`)
	var read strings.Builder
	for offset, windows := 0, 0; ; windows++ {
		if windows > 10 {
			t.Fatal("the message never ended")
		}
		text := renderMessage(message, offset)
		window := text[strings.Index(text, "\n\n")+2:]
		match := continuation.FindStringSubmatch(text)
		if match == nil {
			read.WriteString(strings.TrimSpace(window))
			break
		}
		read.WriteString(window[:strings.Index(window, "\n\n[truncated")])
		next, _ := strconv.Atoi(match[1])
		if next <= offset {
			t.Fatalf("offset did not advance: %d -> %d", offset, next)
		}
		offset = next
	}
	if read.String() != strings.TrimSpace(body) {
		t.Fatalf("windows joined to %d bytes, want %d", read.Len(), len(strings.TrimSpace(body)))
	}
}
