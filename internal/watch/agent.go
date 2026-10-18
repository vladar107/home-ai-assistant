package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/localtime"
)

const (
	// maximumAgentChecks is how many agent checks run at once across all principals; each
	// principal runs one at a time.
	maximumAgentChecks = 2
	// agentResultLifetime is how long a finished check waits to be collected before it is thrown
	// away, such as for a watch paused, muted, or deleted while it ran.
	agentResultLifetime = 10 * time.Minute

	maximumValueRunes    = 200
	maximumHeadlineRunes = 300
	maximumDetailsRunes  = 1000

	agentMet   = "met"
	agentUnmet = "unmet"
)

// CheckRunner runs one unwatched, read-only model turn on the principal's own subscription.
type CheckRunner interface {
	RunCheck(ctx context.Context, principalID, message string) (conversation.ScheduledReply, error)
}

type AgentConfig struct {
	Now func() time.Time
	// Finished is called after each check ends, so the service can collect its result at once
	// rather than at its next tick.
	Finished func()
}

// AgentEvaluator checks agent watches with a model turn each. A turn takes far longer than a code
// check and waits behind its owner's chat turn, so checks run in the background: Evaluate starts a
// due check and returns ErrSkip, and the first Evaluate after it finishes returns what it found.
type AgentEvaluator struct {
	runner   CheckRunner
	now      func() time.Time
	finished func()

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	running map[string]string
	results map[string]agentResult
}

// agentResult is a finished check waiting to be collected. Version is the watch's UpdatedAt when
// the check started, so a check of a watch edited since is thrown away.
type agentResult struct {
	version     time.Time
	finishedAt  time.Time
	observation Observation
	err         error
}

var (
	_ Evaluator = (*AgentEvaluator)(nil)
	_ Stopper   = (*AgentEvaluator)(nil)
)

func NewAgentEvaluator(runner CheckRunner, cfg AgentConfig) (*AgentEvaluator, error) {
	if runner == nil {
		return nil, fmt.Errorf("agent watch runner is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	finished := cfg.Finished
	if finished == nil {
		finished = func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &AgentEvaluator{
		runner:   runner,
		now:      now,
		finished: finished,
		ctx:      ctx,
		cancel:   cancel,
		running:  make(map[string]string),
		results:  make(map[string]agentResult),
	}, nil
}

func (e *AgentEvaluator) Evaluate(_ context.Context, principalID string, watch Watch, occurrence Occurrence) (Observation, error) {
	if watch.Check == nil {
		return Observation{}, fmt.Errorf("the agent watch has no check")
	}
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, result := range e.results {
		if now.Sub(result.finishedAt) > agentResultLifetime {
			delete(e.results, id)
		}
	}
	if result, done := e.results[watch.ID]; done {
		delete(e.results, watch.ID)
		if result.version.Equal(watch.UpdatedAt) {
			return result.observation, result.err
		}
	}
	if e.running[principalID] != "" {
		return Observation{}, ErrSkip
	}
	if !occurrence.Checked.IsZero() && now.Sub(occurrence.Checked) < watch.Check.interval()-checkSlack {
		return Observation{}, ErrSkip
	}
	if len(e.running) >= maximumAgentChecks || e.ctx.Err() != nil {
		return Observation{}, ErrSkip
	}
	e.running[principalID] = watch.ID
	message := checkMessage(watch, occurrence.Previous, now)
	e.wg.Add(1)
	go e.run(principalID, watch, occurrence.Previous, message)
	return Observation{}, ErrSkip
}

func (e *AgentEvaluator) run(principalID string, watch Watch, previous, message string) {
	defer e.wg.Done()
	reply, err := e.runner.RunCheck(e.ctx, principalID, message)
	var observation Observation
	if err == nil {
		observation, err = observe(watch, previous, reply)
	}
	e.mu.Lock()
	delete(e.running, principalID)
	if e.ctx.Err() == nil {
		e.results[watch.ID] = agentResult{version: watch.UpdatedAt, finishedAt: e.now(), observation: observation, err: err}
	}
	e.mu.Unlock()
	e.finished()
}

// Stop cancels the checks still running and waits for them to end.
func (e *AgentEvaluator) Stop(ctx context.Context) error {
	e.cancel()
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// checkMessage is the check turn's instruction: what to look at, when the user wants to hear, the
// value last reported so the model can say what changed, and how to answer.
func checkMessage(watch Watch, previous string, now time.Time) string {
	last := "none yet"
	if _, value, found := strings.Cut(previous, "|"); found && value != "" {
		last = value
	}
	return fmt.Sprintf(
		"[system] This is one check of the user's watch %q, whose times are local to its zone, where it is now %s. The user is not watching this turn and cannot answer questions, "+
			"and only read-only tools are available, so nothing can be created, changed, or saved. "+
			"Do the check below with the tools you have, and never report a value that no tool returned in this run.\n\n"+
			"Check: %s\nTell the user when: %s\nValue last reported: %s\n\n"+
			"End your reply with one JSON object and nothing after it: "+
			`{"met": true or false, "value": "the short value the condition is about, such as a price, a sender, or a date", "headline": "one line for the user saying what you found", "details": "optional short text, such as where you found it"}. `+
			`If you cannot do the check, such as when a tool is missing or a page does not load, end instead with {"error": "what failed"}.`,
		watch.Target.Name, localtime.Format(now, localtime.SavedZone(watch.Zone)), watch.Check.Instruction, watch.Check.Condition, last,
	)
}

type verdict struct {
	Met      *bool  `json:"met"`
	Value    string `json:"value"`
	Headline string `json:"headline"`
	Details  string `json:"details"`
	Error    string `json:"error"`
}

// observe turns a check turn's reply into an observation. The state is met or unmet with the
// normalized value, so the value is there for the next check's message. A changed value is stored
// silently unless the condition holds and the watch alerts on a change, and a change between met
// and unmet is never silent.
func observe(watch Watch, previous string, reply conversation.ScheduledReply) (Observation, error) {
	if reply.ErrorKind != "" {
		return Observation{}, &UpstreamError{Service: "model", Err: fmt.Errorf("the check turn failed: %s", reply.ErrorKind)}
	}
	found, err := parseVerdict(reply.Text)
	if err != nil {
		return Observation{}, err
	}
	if found.Error != "" {
		return Observation{}, fmt.Errorf("the check could not be done: %s", clip.Runes(found.Error, maximumHeadlineRunes))
	}
	met := *found.Met
	key := agentUnmet
	if met {
		key = agentMet
	}
	value := clip.Runes(strings.ToLower(strings.Join(strings.Fields(found.Value), " ")), maximumValueRunes)
	state := key + "|" + value
	previousKey, _, _ := strings.Cut(previous, "|")
	observation := Observation{
		State:     state,
		Triggered: met,
		Headline:  clip.Runes(strings.TrimSpace(found.Headline), maximumHeadlineRunes),
		Private:   reply.Private,
		Silent:    previous != "" && previousKey == key && !(met && wanted(watch, TriggerChange)),
	}
	if details := clip.Runes(strings.TrimSpace(found.Details), maximumDetailsRunes); details != "" {
		observation.Details = []string{details}
	}
	if met && observation.Headline == "" {
		return Observation{}, fmt.Errorf("the check's answer has no headline")
	}
	return observation, nil
}

// parseVerdict reads the JSON object a check turn's reply ends with, allowing a code fence around
// it.
func parseVerdict(text string) (verdict, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	end := strings.LastIndex(text, "}")
	if end < 0 {
		return verdict{}, errors.New("the check's reply has no answer object")
	}
	body := text[:end+1]
	for start := strings.LastIndex(body, "{"); start >= 0; start = strings.LastIndex(body[:start], "{") {
		var found verdict
		if err := json.Unmarshal([]byte(body[start:]), &found); err != nil {
			continue
		}
		if found.Met == nil && strings.TrimSpace(found.Error) == "" {
			return verdict{}, errors.New("the check's answer object has neither met nor error")
		}
		found.Error = strings.TrimSpace(found.Error)
		return found, nil
	}
	return verdict{}, errors.New("the check's reply has no valid answer object")
}
