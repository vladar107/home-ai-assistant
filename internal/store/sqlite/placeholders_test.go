package sqlite

import (
	"bytes"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"
)

var placeholderSuffixPattern = regexp.MustCompile(`^[a-z2-7]{6}$`)

func allocatePlaceholder(t *testing.T, store *Store, principalID, kind, value string) string {
	t.Helper()
	suffix, err := store.PlaceholderToken(t.Context(), principalID, kind, value)
	if err != nil {
		t.Fatal(err)
	}
	if !placeholderSuffixPattern.MatchString(suffix) {
		t.Fatalf("suffix %q is not six base32 characters", suffix)
	}
	return suffix
}

func resolvePlaceholder(t *testing.T, store *Store, principalID, kind, suffix string) (string, bool) {
	t.Helper()
	value, found, err := store.PlaceholderValue(t.Context(), principalID, kind, suffix)
	if err != nil {
		t.Fatal(err)
	}
	return value, found
}

func TestPlaceholderTokenIsStableForAValue(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	suffix := allocatePlaceholder(t, store, "alice", "email", "someone@bank.example")
	if again := allocatePlaceholder(t, store, "alice", "email", "someone@bank.example"); again != suffix {
		t.Fatalf("the same value got %q, then %q", suffix, again)
	}
	phone := allocatePlaceholder(t, store, "alice", "phone", "+31 20 123 4567")
	if phone == suffix {
		t.Fatalf("two values share suffix %q", suffix)
	}
	if value, found := resolvePlaceholder(t, store, "alice", "email", suffix); !found || value != "someone@bank.example" {
		t.Fatalf("resolved %q %v", value, found)
	}
	if _, found := resolvePlaceholder(t, store, "alice", "phone", suffix); found {
		t.Fatal("an email suffix resolved as a phone")
	}

	entries, err := store.PlaceholderEntries(ctx, "alice", "email")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0] != (PlaceholderEntry{Kind: "email", Value: "someone@bank.example", Suffix: suffix}) {
		t.Fatalf("email entries = %+v", entries)
	}
}

func TestPlaceholderTokenAgreesUnderConcurrentAllocation(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	const callers = 16
	distinct := make([]string, callers)
	shared := make([]string, callers)
	errs := make(chan error, 2*callers)
	var wait sync.WaitGroup
	for i := range callers {
		wait.Go(func() {
			suffix, err := store.PlaceholderToken(ctx, "alice", "email", fmt.Sprintf("person%d@example.com", i))
			distinct[i] = suffix
			errs <- err
		})
		wait.Go(func() {
			suffix, err := store.PlaceholderToken(ctx, "alice", "phone", "+31 6 1234 5678")
			shared[i] = suffix
			errs <- err
		})
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]bool{}
	for _, suffix := range distinct {
		if seen[suffix] {
			t.Fatalf("different values share suffix %q: %v", suffix, distinct)
		}
		seen[suffix] = true
	}
	for _, suffix := range shared {
		if suffix != shared[0] {
			t.Fatalf("one value got different suffixes: %v", shared)
		}
	}
	entries, err := store.PlaceholderEntries(ctx, "alice", "phone")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("one value made %d rows", len(entries))
	}
}

func TestPlaceholderTokenRetriesASuffixClash(t *testing.T) {
	t.Parallel()
	store, _ := openTestStore(t)
	zeros, ones := bytes.Repeat([]byte{0}, 6), bytes.Repeat([]byte{1}, 6)
	store.placeholderRandom = bytes.NewReader(bytes.Join([][]byte{zeros, zeros, zeros, ones}, nil))

	if first := allocatePlaceholder(t, store, "alice", "email", "first@example.com"); first != "aaaaaa" {
		t.Fatalf("first suffix = %q", first)
	}
	second := allocatePlaceholder(t, store, "alice", "email", "second@example.com")
	if second != "bbbbbb" {
		t.Fatalf("second suffix = %q, want the draw after two clashes", second)
	}
	if value, found := resolvePlaceholder(t, store, "alice", "email", "aaaaaa"); !found || value != "first@example.com" {
		t.Fatalf("the clash changed the first value: %q %v", value, found)
	}

	store.placeholderRandom = bytes.NewReader(bytes.Repeat(zeros, placeholderSuffixAttempts))
	if _, err := store.PlaceholderToken(t.Context(), "alice", "email", "third@example.com"); err == nil {
		t.Fatal("allocation succeeded with every draw taken")
	}
}

func TestPlaceholdersAreNeverSharedBetweenPrincipals(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	store.placeholderRandom = bytes.NewReader(make([]byte, 12))

	alice := allocatePlaceholder(t, store, "alice", "email", "alice.contact@example.com")
	if _, found := resolvePlaceholder(t, store, "bob", "email", alice); found {
		t.Fatal("bob resolved alice's token")
	}
	bob := allocatePlaceholder(t, store, "bob", "email", "bob.contact@example.com")
	if bob != alice {
		t.Fatalf("suffixes %q and %q; both principals drew the same one", alice, bob)
	}
	if value, found := resolvePlaceholder(t, store, "alice", "email", alice); !found || value != "alice.contact@example.com" {
		t.Fatalf("alice resolved %q %v", value, found)
	}
	if value, found := resolvePlaceholder(t, store, "bob", "email", bob); !found || value != "bob.contact@example.com" {
		t.Fatalf("bob resolved %q %v", value, found)
	}
	entries, err := store.PlaceholderEntries(ctx, "bob", "email")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Value != "bob.contact@example.com" {
		t.Fatalf("bob's entries = %+v", entries)
	}
}

func TestPrunePlaceholdersRemovesOnlyValuesUnseenPastTheCutoff(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := start
	store.SetClock(func() time.Time { return now })

	stale := allocatePlaceholder(t, store, "alice", "email", "stale@example.com")
	resolved := allocatePlaceholder(t, store, "alice", "email", "resolved@example.com")
	reused := allocatePlaceholder(t, store, "alice", "address", "1 Example Street, Exampletown")

	now = start.Add(100 * 24 * time.Hour)
	fresh := allocatePlaceholder(t, store, "alice", "phone", "+31 20 765 4321")
	if _, found := resolvePlaceholder(t, store, "alice", "email", resolved); !found {
		t.Fatal("a value was gone before the prune")
	}
	if again := allocatePlaceholder(t, store, "alice", "address", "1 Example Street, Exampletown"); again != reused {
		t.Fatalf("a reused value changed suffix from %q to %q", reused, again)
	}

	if err := store.PrunePlaceholders(ctx, start.Add(50*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, found := resolvePlaceholder(t, store, "alice", "email", stale); found {
		t.Fatal("a value unseen past the cutoff still resolves")
	}
	for _, kept := range []struct{ kind, suffix string }{{"email", resolved}, {"address", reused}, {"phone", fresh}} {
		if _, found := resolvePlaceholder(t, store, "alice", kept.kind, kept.suffix); !found {
			t.Fatalf("a %s seen after the cutoff was pruned", kept.kind)
		}
	}
}

func TestRevokingAPrincipalDeletesItsPlaceholders(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	alice := allocatePlaceholder(t, store, "alice", "email", "alice.contact@example.com")
	bob := allocatePlaceholder(t, store, "bob", "email", "bob.contact@example.com")

	bootstrap(t, store, configuredAlice)

	if _, found := resolvePlaceholder(t, store, "bob", "email", bob); found {
		t.Fatal("a revoked principal's token still resolves")
	}
	entries, err := store.PlaceholderEntries(ctx, "bob", "email")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a revoked principal kept %+v", entries)
	}
	if _, found := resolvePlaceholder(t, store, "alice", "email", alice); !found {
		t.Fatal("revoking bob removed alice's value")
	}

	if err := store.DeletePlaceholders(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, found := resolvePlaceholder(t, store, "alice", "email", alice); found {
		t.Fatal("a deleted vault still resolves")
	}
}
