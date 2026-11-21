package placeholder_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/placeholder"
)

const alphabet = "abcdefghijklmnopqrstuvwxyz234567"

type vaultKey struct {
	principal string
	kind      placeholder.Kind
	text      string
}

type fakeVault struct {
	mu      sync.Mutex
	next    int
	values  map[vaultKey]string
	entries map[vaultKey]string
	touches map[vaultKey]int
	fail    error
}

func newVault() *fakeVault {
	return &fakeVault{values: map[vaultKey]string{}, entries: map[vaultKey]string{}, touches: map[vaultKey]int{}}
}

func (v *fakeVault) seed(principal string, kind placeholder.Kind, value, suffix string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.entries[vaultKey{principal, kind, value}] = suffix
	v.values[vaultKey{principal, kind, suffix}] = value
}

func (v *fakeVault) Token(_ context.Context, principal string, kind placeholder.Kind, value string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.fail != nil {
		return "", v.fail
	}
	v.touches[vaultKey{principal, kind, value}]++
	if suffix, ok := v.entries[vaultKey{principal, kind, value}]; ok {
		return suffix, nil
	}
	var suffix [6]byte
	for i, n := len(suffix)-1, v.next; i >= 0; i, n = i-1, n/32 {
		suffix[i] = alphabet[n%32]
	}
	v.next++
	v.entries[vaultKey{principal, kind, value}] = string(suffix[:])
	v.values[vaultKey{principal, kind, string(suffix[:])}] = value
	return string(suffix[:]), nil
}

func (v *fakeVault) Value(_ context.Context, principal string, kind placeholder.Kind, suffix string) (string, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.fail != nil {
		return "", false, v.fail
	}
	value, ok := v.values[vaultKey{principal, kind, suffix}]
	return value, ok, nil
}

func (v *fakeVault) Entries(_ context.Context, principal string, kind placeholder.Kind) ([]placeholder.Entry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.fail != nil {
		return nil, v.fail
	}
	var entries []placeholder.Entry
	for key, suffix := range v.entries {
		if key.principal == principal && key.kind == kind {
			entries = append(entries, placeholder.Entry{Kind: kind, Value: key.text, Suffix: suffix})
		}
	}
	return entries, nil
}

func hide(t *testing.T, mapper *placeholder.Mapper, text string, declared ...string) string {
	t.Helper()
	hidden, err := mapper.Hide(context.Background(), text, declared)
	if err != nil {
		t.Fatalf("Hide(%q): %v", text, err)
	}
	return hidden
}

func TestHideReplacesEmailsAndPhones(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Write to jane.doe@example.com today.":         "Write to ⟦email-aaaaaa example.com⟧ today.",
		"From: Bank <alerts@Mail.Bank.Example>":        "From: Bank <⟦email-aaaaaa mail.bank.example⟧>",
		"mailto:john+news@example.org.":                "mailto:⟦email-aaaaaa example.org⟧.",
		"a@example.com, b@example.net, a@example.com":  "⟦email-aaaaaa example.com⟧, ⟦email-aaaaab example.net⟧, ⟦email-aaaaaa example.com⟧",
		"Call +31 20 555 0100 now":                     "Call ⟦phone-aaaaaa⟧ now",
		"Call 020 555 0100.":                           "Call ⟦phone-aaaaaa⟧.",
		"US office: +1 (555) 010-0199":                 "US office: ⟦phone-aaaaaa⟧",
		"London +44 20 7946 0958, desk (020) 555-0100": "London ⟦phone-aaaaaa⟧, desk ⟦phone-aaaaab⟧",
		"Reception +31 (0)20 555 0100":                 "Reception ⟦phone-aaaaaa⟧",
		"Mobile 06-12345678 or 06.12.34.56.78":         "Mobile ⟦phone-aaaaaa⟧ or ⟦phone-aaaaab⟧",
		"tel:+31205550100":                             "tel:⟦phone-aaaaaa⟧",
		"(0205550100)":                                 "(⟦phone-aaaaaa⟧)",
		"0205550100":                                   "⟦phone-aaaaaa⟧",
		"jane@example.com or 020 555 0100":             "⟦email-aaaaaa example.com⟧ or ⟦phone-aaaaab⟧",
		"0612345678@example.com":                       "⟦email-aaaaaa example.com⟧",
		"Call 0612345678 2 times":                      "Call ⟦phone-aaaaaa⟧ 2 times",
		"0201234567 1012 AB":                           "⟦phone-aaaaaa⟧ 1012 AB",
		"+31 20 123 4567 1234 5678":                    "⟦phone-aaaaaa⟧ 1234 5678",
		"0612345678 0687654321":                        "⟦phone-aaaaaa⟧ ⟦phone-aaaaab⟧",
		"0612345678/0687654321":                        "⟦phone-aaaaaa⟧/⟦phone-aaaaab⟧",
		"00 31 20 555 0100":                            "⟦phone-aaaaaa⟧",
		"Tel 0612345678":                               "Tel ⟦phone-aaaaaa⟧",
		"phone: +31 20 555 0100":                       "phone: ⟦phone-aaaaaa⟧",
		"Phone no. 0612345678":                         "Phone no. ⟦phone-aaaaaa⟧",
		"Tel.-Nr. 0612345678":                          "Tel.-Nr. ⟦phone-aaaaaa⟧",
		"Tel.+31 20 123 4567":                          "Tel.⟦phone-aaaaaa⟧",
		"Office 030/1234567":                           "Office ⟦phone-aaaaaa⟧",
		"+31\u00a020\u00a0555\u00a00100":               "⟦phone-aaaaaa⟧",
		"020\u202f555\u202f0100 today":                 "⟦phone-aaaaaa⟧ today",
		"user@localhost, @handle, name@example":        "user@localhost, @handle, name@example",
		"https://social.example/@jane.doe":             "https://social.example/@jane.doe",
	}
	for given, want := range cases {
		mapper := placeholder.New(newVault(), "alice")
		if got := hide(t, mapper, given); got != want {
			t.Errorf("Hide(%q) = %q; want %q", given, got, want)
		}
	}
}

func TestHideLeavesNumbersThatAreNotPhones(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"dates":         {"2026-11-21", "on 21.11.2026", "01/02/2026", "01.02.2026 09:30", "01-02-2026 10:15", "from 2026-11-21 to 2026-11-28", "01.11.2026-05.11.2026"},
		"times":         {"10:30", "08:00-17:00", "at 09:30:15", "09:00 – 10:00"},
		"ranges":        {"10-20", "pages 100-200", "0-100 km", "0100-0200"},
		"tracking":      {"3SABCD123456789", "JJD000123456789012", "00340434161234567890", "1Z999AA10123456784"},
		"iban":          {"NL91 ABNA 0417 1643 00", "NL91ABNA0417164300", "GB82 WEST 1234 5698 7654 32", "nl91 abna 0417 1643 00"},
		"order numbers": {"Order #0012345678", "order ORD-0012345678", "order 2026-0012345", "ref 00123456789012345678", "Lot 000123-000456"},
		"identifiers":   {"Order 0012345678", "Order no. 0012345678", "Klantnummer 0123456789", "build 0123456789", "GLS 05123456789", "ID: 0123456789", "Ref. 0123456789", "tracking:05123456789"},
		"coordinates":   {"+52.3702163", "at +52.3702163, +4.8951679"},
		"other numbers": {"0.25 0.50 0.75 1.00", "price 0,99", "https://example.com/item/0123456789", "?id=0123456789", "555 0100", "+31 20 555"},
	}
	for class, texts := range cases {
		for _, text := range texts {
			mapper := placeholder.New(newVault(), "alice")
			if got := hide(t, mapper, text); got != text {
				t.Errorf("%s: Hide(%q) = %q; want it unchanged", class, text, got)
			}
		}
	}
}

func TestIsAddressAcceptsOnlyStreetShapes(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"12 Example Street":                      true,
		"Building 7, Floor 2":                    true,
		"Example Street 12, 1000 AB Springfield": true,
		"Home":                                   false,
		"Zoom":                                   false,
		"Room 4":                                 false,
		"Example Street":                         false,
		"https://meet.example.com/abc-1234-xyz":  false,
		"meet.example.com/j/123456789":           false,
		"www.example.com/room/12345":             false,
		"tel:+31205550100":                       false,
		"Lindenlaan 12, Westfield":               true,
		"12 Example Street, Springfield":         true,
		"1600 Example Avenue NW":                 true,
		"Platform 12":                            false,
		"Terminal 2":                             false,
		"2026-11-21":                             false,
		"2026-11-21 Example Street":              false,
		"21 November 2026":                       false,
		"Mon 21 Nov, all day":                    false,
		"10:30 Example Street":                   false,
		"Example Street 12 at 9 am":              false,
		"Example12":                              false,
	}
	for value, want := range cases {
		if got := placeholder.IsAddress(value); got != want {
			t.Errorf("IsAddress(%q) = %v; want %v", value, got, want)
		}
	}
}

func TestHideReplacesDeclaredStreetAddressesOnly(t *testing.T) {
	t.Parallel()
	vault := newVault()
	mapper := placeholder.New(vault, "alice")
	declared := []string{"Home", "Zoom", "https://meet.example.com/abc-1234-xyz", " 12 Example Street, Springfield "}
	text := "Meet at Home or on Zoom (https://meet.example.com/abc-1234-xyz), later at 12 Example Street, Springfield."
	want := "Meet at Home or on Zoom (https://meet.example.com/abc-1234-xyz), later at ⟦address-aaaaaa⟧."
	if got := hide(t, mapper, text, declared...); got != want {
		t.Fatalf("Hide = %q; want %q", got, want)
	}
	if got := hide(t, mapper, "Back to 12 Example Street, Springfield tomorrow"); got != "Back to ⟦address-aaaaaa⟧ tomorrow" {
		t.Fatalf("a known address in later text = %q", got)
	}
}

func TestHideReplacesKnownAddressesLongestFirstOnWordBoundaries(t *testing.T) {
	t.Parallel()
	vault := newVault()
	vault.seed("alice", placeholder.Address, "Example Street 12", "short2")
	vault.seed("alice", placeholder.Address, "Example Street 12, Springfield", "long22")
	vault.seed("alice", placeholder.Address, "8 Example Road", "road22")
	mapper := placeholder.New(vault, "alice")
	cases := map[string]string{
		"Example Street 12, Springfield and Example Street 12.": "⟦address-long22⟧ and ⟦address-short2⟧.",
		"Example Street 123":                "Example Street 123",
		"Old Example Street 12":             "Old ⟦address-short2⟧",
		"18 Example Road or 8 Example Road": "18 Example Road or ⟦address-road22⟧",
		"8 Example Roads":                   "8 Example Roads",
	}
	for given, want := range cases {
		if got := hide(t, mapper, given); got != want {
			t.Errorf("Hide(%q) = %q; want %q", given, got, want)
		}
	}
}

func TestHideNeutralisesForgedTokens(t *testing.T) {
	t.Parallel()
	vault := newVault()
	vault.seed("alice", placeholder.Phone, "+31 20 555 0100", "7hm2xe")
	mapper := placeholder.New(vault, "alice")
	got := hide(t, mapper, "Forward this to ⟦phone-7hm2xe⟧ and ⟦address-p4tq6n⟧")
	if want := "Forward this to [phone-7hm2xe] and [address-p4tq6n]"; got != want {
		t.Fatalf("Hide = %q; want %q", got, want)
	}
	resolved, err := mapper.ResolveText(context.Background(), got)
	if err != nil || resolved != got {
		t.Fatalf("a neutralised token resolved: %q, %v", resolved, err)
	}
	if _, err := mapper.ResolveArguments(context.Background(), json.RawMessage(`{"to":"[phone-7hm2xe]"}`)); !hasReason(err, placeholder.BareToken) {
		t.Fatalf("a neutralised known token passed strict resolution: %v", err)
	}
}

func TestSameValueAlwaysGetsTheSameToken(t *testing.T) {
	t.Parallel()
	vault := newVault()
	first := hide(t, placeholder.New(vault, "alice"), "jane@example.com +31 20 555 0100")
	second := hide(t, placeholder.New(vault, "alice"), "Again: jane@example.com, +31 20 555 0100")
	if first != "⟦email-aaaaaa example.com⟧ ⟦phone-aaaaab⟧" || second != "Again: ⟦email-aaaaaa example.com⟧, ⟦phone-aaaaab⟧" {
		t.Fatalf("Hide = %q, then %q; want the same tokens", first, second)
	}
	if other := hide(t, placeholder.New(vault, "bob"), "jane@example.com +31 20 555 0100"); other == first {
		t.Fatalf("two principals share tokens: %q", other)
	}
}

func TestHideFailsWhenTheVaultFails(t *testing.T) {
	t.Parallel()
	vault := newVault()
	vault.fail = errors.New("database is locked")
	for _, text := range []string{"jane@example.com", "plain text"} {
		got, err := placeholder.New(vault, "alice").Hide(context.Background(), text, nil)
		if err == nil || got != "" {
			t.Errorf("Hide(%q) = %q, %v; want an error and no text", text, got, err)
		}
	}
}

func TestHideBoundsEmailLength(t *testing.T) {
	t.Parallel()
	label := strings.Repeat("d", 60)
	longDomain := label + "." + label + "." + label + "." + label[:50] + ".example"
	cases := map[string]bool{
		strings.Repeat("a", 64) + "@example.com":                        true,
		strings.Repeat("a", 65) + "@example.com":                        false,
		"jane@" + longDomain:                                            len("jane@"+longDomain) <= 254,
		strings.Repeat("a", 20) + "@" + longDomain:                      false,
		strings.Repeat("a", 254-len("@"+longDomain)) + "@" + longDomain: true,
	}
	for email, hidden := range cases {
		got := hide(t, placeholder.New(newVault(), "alice"), "Mail "+email)
		if hidden != strings.HasPrefix(got, "Mail ⟦email-") || !hidden && got != "Mail "+email {
			t.Errorf("Hide of a %d-character address = %q; want hidden %v", len(email), got, hidden)
		}
	}
}

func TestHideRegistersTheFirstPartOfADeclaredAddress(t *testing.T) {
	t.Parallel()
	mapper := placeholder.New(newVault(), "alice")
	got := hide(t, mapper, "At 12 Example Street, Springfield; later at 12 Example Street. Main Hall, 2nd floor.", "12 Example Street, Springfield", "Main Hall, 2nd floor")
	if want := "At ⟦address-aaaaaa⟧; later at ⟦address-aaaaab⟧. ⟦address-aaaaac⟧."; got != want {
		t.Fatalf("Hide = %q; want %q", got, want)
	}
	// The part of a street address before its comma is registered even when it would not pass as
	// an address alone, since a trip line quotes only that part.
	got = hide(t, mapper, "Now at Lindenlaan 12. Platform 12 stays.", "Lindenlaan 12, Westfield", "Platform 12")
	if want := "Now at ⟦address-aaaaae⟧. Platform 12 stays."; got != want {
		t.Fatalf("Hide = %q; want %q", got, want)
	}
}

func TestHideTouchesKnownAddressesItReplaces(t *testing.T) {
	t.Parallel()
	vault := newVault()
	vault.seed("alice", placeholder.Address, "12 Example Street", "p4tq6n")
	vault.seed("alice", placeholder.Address, "8 Example Road", "road22")
	mapper := placeholder.New(vault, "alice")
	key := vaultKey{"alice", placeholder.Address, "12 Example Street"}
	for want := 1; want <= 2; want++ {
		hide(t, mapper, "From 12 Example Street back to 12 Example Street")
		if got := vault.touches[key]; got != want {
			t.Fatalf("touches after %d Hide calls = %d", want, got)
		}
	}
	if got := vault.touches[vaultKey{"alice", placeholder.Address, "8 Example Road"}]; got != 0 {
		t.Fatalf("an address the text did not hold was touched %d times", got)
	}
}

func seededMapper(principal string) *placeholder.Mapper {
	vault := newVault()
	vault.seed("alice", placeholder.Email, "Jane.Doe@Example.com", "k3f7qa")
	vault.seed("alice", placeholder.Phone, "+31 20 555 0100", "7hm2xe")
	vault.seed("alice", placeholder.Address, "12 Example Street", "p4tq6n")
	vault.seed("alice", placeholder.Address, "Example Lane 3", "change")
	return placeholder.New(vault, principal)
}

func hasReason(err error, reason placeholder.Reason) bool {
	var resolveErr *placeholder.ResolveError
	return errors.As(err, &resolveErr) && resolveErr.Reason == reason
}

func TestResolveArgumentsReplacesKnownTokens(t *testing.T) {
	t.Parallel()
	mapper := seededMapper("alice")
	raw := `{"to":["⟦email-k3f7qa example.com⟧"],"cc":"⟦email-k3f7qa EXAMPLE.COM⟧","body":"Call ⟦phone-7hm2xe⟧ <now> & visit ⟦address-p4tq6n⟧","count":12345678901234567890,"draft":true,"thread":null,"note":"phone-number, email-thread"}`
	got, err := mapper.ResolveArguments(context.Background(), json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(got)))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	want := map[string]any{
		"to":     []any{"Jane.Doe@Example.com"},
		"cc":     "Jane.Doe@Example.com",
		"body":   "Call +31 20 555 0100 <now> & visit 12 Example Street",
		"count":  json.Number("12345678901234567890"),
		"draft":  true,
		"thread": nil,
		"note":   "phone-number, email-thread",
	}
	gotJSON, _ := json.Marshal(decoded)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("ResolveArguments = %s; want %s", gotJSON, wantJSON)
	}
	plain := json.RawMessage(`{ "b": 1.50,  "a": "x <y> address-update" }`)
	if got, err := mapper.ResolveArguments(context.Background(), plain); err != nil || string(got) != string(plain) {
		t.Fatalf("arguments without tokens = %s, %v; want them as given", got, err)
	}
}

func TestResolveArgumentsRefusesTokensItCannotResolve(t *testing.T) {
	t.Parallel()
	cases := map[string]placeholder.Reason{
		`{"to":"⟦phone-zzzzzz⟧"}`:                        placeholder.UnknownToken,
		`{"to":"⟦address-aaaaaa⟧"}`:                      placeholder.UnknownToken,
		`{"to":"⟦email-7hm2xe example.com⟧"}`:            placeholder.UnknownToken,
		`{"to":"⟦email-k3f7qa⟧"}`:                        placeholder.MalformedToken,
		`{"to":"⟦phone-7hm2x⟧"}`:                         placeholder.MalformedToken,
		`{"to":"⟦address-p4tq6n"}`:                       placeholder.MalformedToken,
		`{"to":"⟦phone-7hm2xe example.com⟧"}`:            placeholder.MalformedToken,
		`{"to":"⟦Email-k3f7qa example.com⟧"}`:            placeholder.MalformedToken,
		`{"to":"email-k3f7qa"}`:                          placeholder.BareToken,
		`{"to":"email-k3f7qa example.com"}`:              placeholder.BareToken,
		`{"body":["Call phone-7hm2xe today"]}`:           placeholder.BareToken,
		`{"where":{"place":"address-p4tq6n"}}`:           placeholder.BareToken,
		`{"where":"address-change"}`:                     placeholder.BareToken,
		`{"to":"⟦email-k3f7qa other.example⟧"}`:          placeholder.DomainMismatch,
		`{"ok":"⟦phone-7hm2xe⟧","bad":"⟦phone-zzzzzz⟧"}`: placeholder.UnknownToken,
		`{"⟦phone-7hm2xe⟧":"call"}`:                      placeholder.TokenInKey,
		`{"list":[{"⟦email-k3f7qa example.com⟧":1}]}`:    placeholder.TokenInKey,
		`{"phone-7hm2xe":"call"}`:                        placeholder.BareToken,
	}
	mapper := seededMapper("alice")
	for raw, reason := range cases {
		got, err := mapper.ResolveArguments(context.Background(), json.RawMessage(raw))
		if !hasReason(err, reason) {
			t.Errorf("ResolveArguments(%s) = %s, %v; want a %s refusal", raw, got, err, reason)
		}
	}
	for _, raw := range []string{`{"to":`, `{"to":"a"} {"to":"b"}`, `{"to":"⟦phone-7hm2xe⟧"}x`, `"a" "b"`} {
		var resolveErr *placeholder.ResolveError
		if got, err := mapper.ResolveArguments(context.Background(), json.RawMessage(raw)); err == nil || errors.As(err, &resolveErr) {
			t.Errorf("ResolveArguments(%s) = %s, %v; want a decode error", raw, got, err)
		}
	}
	if got, err := mapper.ResolveArguments(context.Background(), json.RawMessage("{\"to\":\"⟦phone-7hm2xe⟧\"}\n")); err != nil || string(got) != `{"to":"+31 20 555 0100"}` {
		t.Errorf("arguments with trailing whitespace = %s, %v", got, err)
	}
}

func TestTokensResolveOnlyForTheirPrincipal(t *testing.T) {
	t.Parallel()
	mapper := seededMapper("bob")
	if _, err := mapper.ResolveArguments(context.Background(), json.RawMessage(`{"to":"⟦phone-7hm2xe⟧"}`)); !hasReason(err, placeholder.UnknownToken) {
		t.Fatalf("another principal's token = %v; want an unknown token refusal", err)
	}
	if got, err := mapper.ResolveText(context.Background(), "Call ⟦phone-7hm2xe⟧"); err != nil || got != "Call (hidden number)" {
		t.Fatalf("another principal's token in text = %q, %v", got, err)
	}
}

func TestResolveTextRendersMarkersForUnknownTokens(t *testing.T) {
	t.Parallel()
	mapper := seededMapper("alice")
	text := "Reply to ⟦email-k3f7qa example.com⟧ or call ⟦phone-7hm2xe⟧ at ⟦address-p4tq6n⟧. " +
		"Gone: ⟦email-zzzzzz example.com⟧ ⟦phone-zzzzzz⟧ ⟦address-zzzzzz⟧. Forged domain: ⟦email-k3f7qa other.example⟧. " +
		"Left alone: phone-7hm2xe, ⟦phone-7hm2x⟧."
	want := "Reply to Jane.Doe@Example.com or call +31 20 555 0100 at 12 Example Street. " +
		"Gone: (hidden address) (hidden number) (hidden place). Forged domain: (hidden address). " +
		"Left alone: phone-7hm2xe, ⟦phone-7hm2x⟧."
	got, err := mapper.ResolveText(context.Background(), text)
	if err != nil || got != want {
		t.Fatalf("ResolveText = %q, %v; want %q", got, err, want)
	}
}

func stream(t *testing.T, resolver *placeholder.StreamResolver, chunks ...string) []string {
	t.Helper()
	var outputs []string
	for _, chunk := range chunks {
		out, err := resolver.Write(chunk)
		if err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
		outputs = append(outputs, out)
	}
	out, err := resolver.Flush()
	if err != nil {
		t.Fatal(err)
	}
	return append(outputs, out)
}

func TestStreamResolvesTokensSplitAcrossChunks(t *testing.T) {
	t.Parallel()
	mapper := seededMapper("alice")
	cases := []struct {
		chunks []string
		want   []string
	}{
		{[]string{"Call ⟦pho", "ne-7hm2xe⟧ now"}, []string{"Call ", "+31 20 555 0100 now", ""}},
		{[]string{"Mail ⟦email-k3", "f7qa exam", "ple.com⟧."}, []string{"Mail ", "", "Jane.Doe@Example.com.", ""}},
		{[]string{"At ⟦address-p4tq6n⟧, ⟦phone-zzzzzz⟧"}, []string{"At 12 Example Street, (hidden number)", ""}},
		{[]string{"x" + "⟦"[:1], "⟦"[1:] + "phone-7hm2xe⟧"}, []string{"x", "+31 20 555 0100", ""}},
		{[]string{"done ⟦pho"}, []string{"done ", "⟦pho"}},
		{[]string{"a [b] ⟦ ⟦phone-7hm2xe⟧"}, []string{"a [b] ⟦ +31 20 555 0100", ""}},
	}
	for _, c := range cases {
		got := stream(t, mapper.Stream(context.Background()), c.chunks...)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("stream %q = %q; want %q", c.chunks, got, c.want)
		}
	}
}

func TestStreamReleasesAStrayBracketAfterTheCap(t *testing.T) {
	t.Parallel()
	resolver := seededMapper("alice").Stream(context.Background())
	if out, _ := resolver.Write("Price ⟦"); out != "Price " {
		t.Fatalf("first chunk = %q", out)
	}
	long := strings.Repeat("x", 266)
	if out, _ := resolver.Write(long); out != "" {
		t.Fatalf("text under the cap was released: %q", out)
	}
	if out, _ := resolver.Write("yz"); out != "⟦"+long+"y"+"z" {
		t.Fatalf("text at the cap = %q; want the held text released", out)
	}
	if out, _ := resolver.Write(" then ⟦phone-7hm2xe⟧"); out != " then +31 20 555 0100" {
		t.Fatalf("a token after a released bracket = %q", out)
	}
}

func TestStreamResolvesTheLongestToken(t *testing.T) {
	t.Parallel()
	domain := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	vault := newVault()
	vault.seed("alice", placeholder.Email, "x@"+domain, "long22")
	token := "⟦email-long22 " + domain + "⟧"
	chunks := []string{"To " + token[:100], token[100:200], token[200:] + "."}
	got := stream(t, placeholder.New(vault, "alice").Stream(context.Background()), chunks...)
	if strings.Join(got, "") != "To x@"+domain+"." {
		t.Fatalf("stream of a %d-character token = %q", len([]rune(token)), got)
	}
}

func TestStreamFailsWhenTheVaultFails(t *testing.T) {
	t.Parallel()
	vault := newVault()
	vault.fail = errors.New("database is locked")
	resolver := placeholder.New(vault, "alice").Stream(context.Background())
	if out, err := resolver.Write("Call ⟦phone-7hm2xe⟧"); err == nil || out != "" {
		t.Fatalf("Write = %q, %v; want an error and no text", out, err)
	}
}
