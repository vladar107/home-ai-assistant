package googlesim

import (
	"testing"
	"time"
)

func TestMatchesQuery(t *testing.T) {
	zone := time.FixedZone("test", 3600)
	now := time.Date(2026, 3, 12, 8, 0, 0, 0, zone)
	invoice := Message{
		From: "Northwind Energy <billing@northwind.example>", To: "sam@example.test", Subject: "Invoice 2026-02",
		Snippet: "Please pay by the 15th", Labels: []string{"INBOX", "UNREAD"}, Received: now.Add(-10 * time.Hour),
	}
	digest := Message{
		From: "Film Digest <digest@filmdigest.example>", To: "sam@example.test", Subject: "This month's picks",
		Labels: []string{"INBOX"}, Received: now.Add(-5 * 24 * time.Hour),
	}
	tests := []struct {
		query          string
		invoice, other bool
	}{
		{"", true, true},
		{"invoice", true, false},
		{"Invoice Northwind", true, false},
		{"is:unread", true, false},
		{"is:read", false, true},
		{"from:northwind", true, false},
		{"from:digest@filmdigest.example", false, true},
		{"to:sam@example.test", true, true},
		{"subject:invoice", true, false},
		{`subject:"this month"`, false, true},
		{`"pay by the 15th"`, true, false},
		{"newer_than:1d", true, false},
		{"newer_than:7d", true, true},
		{"older_than:2d", false, true},
		{"after:2026/03/12", false, false},
		{"after:2026/03/11", true, false},
		{"after:2026-03-01 before:2026/03/12", true, true},
		{"before:2026/03/13", true, true},
		{"in:inbox", true, true},
		{"in:sent", false, false},
		{"label:unread", true, false},
		{"in:inbox -from:digest", true, false},
		{"invoice OR picks", true, true},
		{"from:northwind OR from:digest", true, true},
		{"{invoice picks}", true, true},
		{"(invoice OR picks) is:unread", true, false},
		{"newer_than:1d is:unread in:inbox", true, false},
		{"newer_than:soon", false, false},
		{"unrelated", false, false},
	}
	for _, tt := range tests {
		if got := matchesQuery(invoice, tt.query, now); got != tt.invoice {
			t.Errorf("%q on the invoice: got %v, want %v", tt.query, got, tt.invoice)
		}
		if got := matchesQuery(digest, tt.query, now); got != tt.other {
			t.Errorf("%q on the digest: got %v, want %v", tt.query, got, tt.other)
		}
	}
}
