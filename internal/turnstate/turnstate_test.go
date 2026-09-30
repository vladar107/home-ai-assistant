package turnstate

import (
	"fmt"
	"slices"
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

func TestMessagesAndMarksExpireAfterTheWindow(t *testing.T) {
	r, c := newTestRegistry()
	r.RecordMessage("alice", "old")
	r.MarkExposed("alice")
	c.advance(20 * time.Minute)
	r.RecordMessage("alice", "newer")
	c.advance(15 * time.Minute)
	if got := r.RecentMessages("alice"); !slices.Equal(got, []string{"newer"}) {
		t.Fatalf("messages = %v", got)
	}
	if got := r.Exposure("alice"); got != LevelNone {
		t.Fatalf("exposure = %v", got)
	}
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
				r.MarkExposed(id)
				r.MarkSuspicious(id)
				r.RecentMessages(id)
				r.Exposure(id)
			}
		}()
	}
	wg.Wait()
	if got := r.Exposure("p0"); got != LevelSuspicious {
		t.Fatalf("exposure = %v", got)
	}
}
