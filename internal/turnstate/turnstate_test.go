package turnstate

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestRegistry() (*Registry, *clock) {
	c := &clock{now: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	return New(c.Now), c
}

func TestMessagesKeepTheLastTwoOldestFirst(t *testing.T) {
	r, _ := newTestRegistry()
	r.RecordMessage("alice", "one")
	r.RecordMessage("alice", "  ")
	r.RecordMessage("alice", "")
	r.RecordMessage("alice", "two")
	r.RecordMessage("alice", "three")
	if got := r.RecentMessages("alice"); !slices.Equal(got, []string{"two", "three"}) {
		t.Fatalf("messages = %v", got)
	}
	if got := r.RecentMessages("bob"); len(got) != 0 {
		t.Fatalf("other principal's messages = %v", got)
	}
}

func TestExchangeInterleavesTheLatestReplyInOrder(t *testing.T) {
	r, _ := newTestRegistry()
	r.RecordReply("alice", "too early")
	r.RecordMessage("alice", "one")
	r.RecordReply("alice", "first answer")
	r.RecordReply("alice", "Shall I add bread?")
	r.RecordReply("alice", " ")
	r.RecordMessage("alice", "yes")
	want := []Line{{RoleUser, "one"}, {RoleAssistant, "Shall I add bread?"}, {RoleUser, "yes"}}
	if got := r.RecentExchange("alice"); !slices.Equal(got, want) {
		t.Fatalf("exchange = %v", got)
	}
	r.RecordReply("alice", "Added.")
	want = []Line{{RoleUser, "one"}, {RoleUser, "yes"}, {RoleAssistant, "Added."}}
	if got := r.RecentExchange("alice"); !slices.Equal(got, want) {
		t.Fatalf("exchange = %v", got)
	}
	r.RecordMessage("alice", "thanks")
	want = []Line{{RoleUser, "yes"}, {RoleAssistant, "Added."}, {RoleUser, "thanks"}}
	if got := r.RecentExchange("alice"); !slices.Equal(got, want) {
		t.Fatalf("exchange = %v", got)
	}
	if got := r.RecentMessages("alice"); !slices.Equal(got, []string{"yes", "thanks"}) {
		t.Fatalf("messages = %v", got)
	}
	if got := r.RecentExchange("bob"); len(got) != 0 {
		t.Fatalf("other principal's exchange = %v", got)
	}
}

func TestReplyKeepsItsEnd(t *testing.T) {
	r, _ := newTestRegistry()
	r.RecordReply("alice", "x"+strings.Repeat("é", MaxReplyRunes))
	got := r.RecentExchange("alice")
	if len(got) != 1 || got[0].Text != strings.Repeat("é", MaxReplyRunes) {
		t.Fatalf("exchange = %d lines, first %d runes", len(got), len([]rune(got[0].Text)))
	}
}

func TestMessagesAndMarksExpireAfterTheWindow(t *testing.T) {
	r, c := newTestRegistry()
	r.RecordMessage("alice", "old")
	r.RecordReply("alice", "old reply")
	r.MarkExposed("alice")
	c.advance(20 * time.Minute)
	r.RecordMessage("alice", "newer")
	c.advance(15 * time.Minute)
	if got := r.RecentMessages("alice"); !slices.Equal(got, []string{"newer"}) {
		t.Fatalf("messages = %v", got)
	}
	if got := r.RecentExchange("alice"); !slices.Equal(got, []Line{{RoleUser, "newer"}}) {
		t.Fatalf("exchange = %v", got)
	}
	if got := r.Exposure("alice"); got != LevelNone {
		t.Fatalf("exposure = %v", got)
	}
	r.RecordReply("alice", "newest reply")
	c.advance(Window)
	r.RecentMessages("alice")
	if len(r.principals) != 0 {
		t.Fatalf("expired principals kept: %d", len(r.principals))
	}
}

func TestSuspiciousOutranksExposedAndImpliesIt(t *testing.T) {
	r, c := newTestRegistry()
	if got := r.Exposure("alice"); got != LevelNone {
		t.Fatalf("exposure = %v", got)
	}
	r.MarkExposed("alice")
	if got := r.Exposure("alice"); got != LevelExposed {
		t.Fatalf("exposure = %v", got)
	}
	c.advance(time.Minute)
	r.MarkSuspicious("alice")
	r.MarkExposed("alice")
	if got := r.Exposure("alice"); got != LevelSuspicious {
		t.Fatalf("exposure = %v", got)
	}
	r.MarkSuspicious("bob")
	if got := r.Exposure("bob"); got != LevelSuspicious {
		t.Fatalf("exposure = %v", got)
	}
}

func TestConcurrentUse(t *testing.T) {
	r := New(nil)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("p%d", i%3)
			for range 100 {
				r.RecordMessage(id, "hello")
				r.RecordReply(id, "hi")
				r.MarkExposed(id)
				r.MarkSuspicious(id)
				r.RecentMessages(id)
				r.RecentExchange(id)
				r.Exposure(id)
			}
		}()
	}
	wg.Wait()
	if got := r.Exposure("p0"); got != LevelSuspicious {
		t.Fatalf("exposure = %v", got)
	}
}
