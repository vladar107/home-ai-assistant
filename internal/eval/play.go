package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

const (
	idlePoll        = 5 * time.Millisecond
	defaultStepWait = 5 * time.Minute
	thumbsDown      = "👎"
)

// Proposal is one change a turn offered, with where it stands once the step ended.
type Proposal struct {
	ID        string          `json:"id"`
	BundleID  string          `json:"bundle_id"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Summary   string          `json:"summary"`
	Status    string          `json:"status"`
	Outcome   string          `json:"outcome,omitempty"`
}

// TurnRecord is what one step produced, captured at the boundaries the stub and the recorder own.
type TurnRecord struct {
	Kind string `json:"kind"`
	// Message is what the model was sent for the step's first run: the composed turn message.
	Message string `json:"message,omitempty"`
	Reply   string `json:"reply"`
	// Quiet marks a scheduled run that had nothing to say.
	Quiet bool `json:"quiet,omitempty"`
	// Proposals are the changes offered during the step; Resolved are the proposals a Confirm or
	// Reject acted on, as they stand afterwards.
	Proposals []Proposal `json:"proposals,omitempty"`
	Resolved  []Proposal `json:"resolved,omitempty"`
	// Reconsider is how a 👎 step's reconsider turn started.
	Reconsider string                   `json:"reconsider,omitempty"`
	ToolCalls  []ToolCall               `json:"tool_calls,omitempty"`
	Captured   Captured                 `json:"captured"`
	Runs       []RunRecord              `json:"-"`
	Usage      agentruntime.Usage       `json:"usage"`
	Tokens     agentruntime.TokenCounts `json:"tokens"`
	Model      string                   `json:"model,omitempty"`
}

// Play runs one step of the case and returns what it produced. Steps are played one at a time.
func (w *World) Play(ctx context.Context, step Step) (TurnRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, cmpDuration(w.options.StepTimeout, defaultStepWait))
	defer cancel()

	w.Chat.Drain()
	mark := w.Recorder.Mark()
	record := TurnRecord{}
	var err error
	switch {
	case step.Say != "":
		record.Kind = "say"
		err = w.say(ctx, step.Say)
	case step.Confirm || step.Reject:
		record.Kind = "confirm"
		if step.Reject {
			record.Kind = "reject"
		}
		record.Resolved, err = w.press(ctx, step.Confirm)
	case step.ThumbsDown:
		record.Kind = "thumbs_down"
		record.Reconsider, err = w.react(ctx)
	case step.RunJob != "":
		record.Kind = "run_job"
		record.Quiet, err = w.runJob(ctx, step.RunJob)
	case step.TriageMail:
		record.Kind = "triage_mail"
		err = w.triageMail(ctx)
	default:
		return TurnRecord{}, errors.New("step has no action")
	}
	record.Captured = w.Chat.Drain()
	record.Runs = w.Recorder.Since(mark)
	w.collect(ctx, &record)
	if err != nil {
		return record, err
	}
	w.pending = slices.Clone(record.Captured.Offers)
	if id := record.Captured.lastReplyID(); id != 0 {
		w.lastReply = id
	}
	// A manual run with nothing to say is still delivered, with the scheduler's own line saying so,
	// so the quiet run is recognised by what the model answered.
	if record.Kind == "run_job" && (record.Quiet || quietAnswer(record.Runs) || quietManual(record.Captured)) {
		record.Quiet, record.Reply = true, ""
	}
	if record.Kind == "triage_mail" {
		record.Quiet = len(record.Captured.Replies) == 0
	}
	return record, nil
}

// triageMail polls the assistant mailbox once so it starts reading, delivers the case's mail, and
// polls again, waiting each time for the triage the poll started; the notice, if any, is the
// step's reply. The mail is delivered once, so a later step triages only what arrives after it.
func (w *World) triageMail(ctx context.Context) error {
	mailbox := w.Assembly.AssistantMail
	if mailbox == nil || w.assistantMail == nil {
		return errors.New("triage_mail needs an assistant_mail section")
	}
	poll := func() error {
		if err := mailbox.Poll(ctx); err != nil {
			return fmt.Errorf("poll the assistant mailbox: %w", err)
		}
		return mailbox.WaitTriage(ctx)
	}
	if err := poll(); err != nil {
		return err
	}
	if err := w.deliverAssistantMail(); err != nil {
		return err
	}
	return poll()
}

func quietAnswer(runs []RunRecord) bool {
	if len(runs) == 0 {
		return false
	}
	return prompt.Silent(runs[len(runs)-1].Result.Response)
}

// quietManual recognises the quiet run of a job that runs no model turn of its own, such as Loose
// ends, by the scheduler's own line, matched in English, the cases' default language.
func quietManual(captured Captured) bool {
	return len(captured.Replies) == 1 && strings.HasSuffix(strings.TrimSpace(captured.Replies[0].Text), schedule.QuietManualText)
}

func cmpDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func (w *World) nextID() int64 {
	w.counter++
	return w.counter
}

func (w *World) enqueue(ctx context.Context, message string, replyTo, messageID int64) error {
	if messageID == 0 {
		messageID = w.Chat.NextID()
	}
	err := w.Assembly.Conversation.Enqueue(ctx, conversation.EnqueueInput{
		BotUserID: botUserID, TelegramUpdateID: w.nextID(), TelegramMessageID: messageID,
		PrincipalID: PrincipalID, ChatID: w.chatID, Message: message, ReplyToMessageID: replyTo,
	})
	if err != nil {
		return fmt.Errorf("enqueue the turn: %w", err)
	}
	return nil
}

func (w *World) say(ctx context.Context, text string) error {
	if err := w.enqueue(ctx, text, 0, 0); err != nil {
		return err
	}
	return w.waitIdle(ctx)
}

// react puts a 👎 on the previous reply, as the Telegram reaction handler does.
func (w *World) react(ctx context.Context) (string, error) {
	if w.lastReply == 0 {
		return "", errors.New("thumbs_down needs a previous reply to react to")
	}
	reacted, err := w.Assembly.Conversation.React(ctx, conversation.ReactionInput{
		BotUserID: botUserID, TelegramUpdateID: w.nextID(), PrincipalID: PrincipalID, ChatID: w.chatID,
		MessageID: w.lastReply, Emojis: []string{thumbsDown},
	})
	if err != nil {
		return "", err
	}
	if !reacted.Recorded {
		return "", errors.New("the previous reply was not recorded as a reply")
	}
	if reacted.Reconsider != conversation.ReconsiderStarted {
		return reacted.Reconsider, fmt.Errorf("the 👎 started no reconsider turn: %q", reacted.Reconsider)
	}
	return reacted.Reconsider, w.waitIdle(ctx)
}

// press confirms or rejects every proposal the previous turn offered, as a button tap does, and
// continues the conversation from the outcome when a confirmation ran its actions.
func (w *World) press(ctx context.Context, confirm bool) ([]Proposal, error) {
	if len(w.pending) == 0 {
		return nil, errors.New("the previous turn offered nothing to confirm or reject")
	}
	action := approval.ActionReject
	if confirm {
		action = approval.ActionConfirm
	}
	var resolved []Proposal
	var ran bool
	var anchor int64
	for _, offer := range w.pending {
		data := offer.Reject
		if confirm {
			data = offer.Confirm
		}
		_, approvalID, ok := approval.ParseCallback(data)
		if !ok {
			return nil, fmt.Errorf("the proposal message %d has no %s button", offer.MessageID, action)
		}
		pressed, err := w.Assembly.Approvals.Press(ctx, PrincipalID, action, approvalID)
		if err != nil {
			return nil, err
		}
		if !pressed.Owned {
			return nil, fmt.Errorf("proposal %s is not the eval principal's", approvalID)
		}
		ran, anchor = ran || pressed.Ran, offer.MessageID
		bundle, err := w.proposals(ctx, approvalID)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, bundle...)
	}
	if ran {
		if err := w.enqueue(ctx, prompt.Continue, 0, anchor); err != nil {
			return resolved, err
		}
	}
	return resolved, w.waitIdle(ctx)
}

// runJob asks the schedule service to run the case's job now and waits for the run to be delivered
// or skipped. The scheduler runs only for that wait, so nothing else it holds runs behind the
// step's back at the fixed clock.
func (w *World) runJob(ctx context.Context, caseID string) (bool, error) {
	jobID, ok := w.jobIDs[caseID]
	if !ok {
		return false, fmt.Errorf("the world has no job %q", caseID)
	}
	scheduler := w.Assembly.Scheduler
	known, err := w.runIDs(ctx, jobID)
	if err != nil {
		return false, err
	}
	if err := scheduler.Start(ctx); err != nil {
		return false, err
	}
	w.scheduling = true
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = scheduler.Shutdown(stopCtx)
		w.scheduling = false
	}()
	queued, err := scheduler.RunNow(ctx, PrincipalID, jobID, fmt.Sprintf("eval-%d", w.nextID()))
	if err != nil || !queued {
		return false, fmt.Errorf("queue the job %q: queued=%v: %w", caseID, queued, err)
	}
	status, errorKind, err := w.awaitRun(ctx, jobID, known)
	if err != nil {
		return false, err
	}
	switch status {
	case schedule.StatusDelivered:
		return false, nil
	case schedule.StatusSkipped:
		return true, nil
	}
	return false, fmt.Errorf("the run of job %q ended %s (%s)", caseID, status, errorKind)
}

func (w *World) runIDs(ctx context.Context, jobID string) ([]string, error) {
	rows, err := w.db.QueryContext(ctx, `SELECT id FROM schedule_runs WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (w *World) awaitRun(ctx context.Context, jobID string, known []string) (status, errorKind string, err error) {
	ticker := time.NewTicker(idlePoll)
	defer ticker.Stop()
	for {
		rows, err := w.db.QueryContext(ctx, `SELECT id, status, COALESCE(error_kind, '') FROM schedule_runs WHERE job_id = ?`, jobID)
		if err != nil {
			return "", "", err
		}
		for rows.Next() {
			var id, runStatus, kind string
			if err := rows.Scan(&id, &runStatus, &kind); err != nil {
				_ = rows.Close()
				return "", "", err
			}
			if slices.Contains(known, id) {
				continue
			}
			switch runStatus {
			case schedule.StatusDelivered, schedule.StatusSkipped, schedule.StatusFailed:
				_ = rows.Close()
				return runStatus, kind, nil
			}
		}
		if err := rows.Err(); err != nil {
			return "", "", err
		}
		_ = rows.Close()
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("wait for the scheduled run: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// waitIdle waits until the principal has no turn queued, running or being delivered and no
// proposal waits to be sent, which happens just after the turn's reply.
func (w *World) waitIdle(ctx context.Context) error {
	ticker := time.NewTicker(idlePoll)
	defer ticker.Stop()
	for {
		var active int
		err := w.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM conversation_requests WHERE principal_id = ? AND state IN ('queued', 'running', 'ready', 'delivering')`,
			PrincipalID).Scan(&active)
		if err != nil {
			return err
		}
		if active == 0 {
			var unsent int
			if err := w.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM approvals WHERE principal_id = ? AND mode = 'proposed' AND status = 'pending' AND message_id IS NULL`,
				PrincipalID).Scan(&unsent); err != nil {
				return err
			}
			if unsent == 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the turn to be delivered: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// proposals loads the bundle an approval belongs to.
func (w *World) proposals(ctx context.Context, approvalID string) ([]Proposal, error) {
	record, found, err := w.Store.LoadApproval(ctx, approvalID)
	if err != nil || !found {
		return nil, fmt.Errorf("load approval %s: found=%v: %w", approvalID, found, err)
	}
	bundle, err := w.Store.LoadBundle(ctx, record.BundleID)
	if err != nil {
		return nil, err
	}
	proposals := make([]Proposal, 0, len(bundle))
	for _, item := range bundle {
		proposals = append(proposals, Proposal{
			ID: item.ID, BundleID: item.BundleID, Tool: item.Tool, Arguments: item.Arguments,
			Summary: item.Summary, Status: item.Status, Outcome: item.Outcome,
		})
	}
	return proposals, nil
}

// collect fills the record's derived fields from what the stub and the recorder captured.
func (w *World) collect(ctx context.Context, record *TurnRecord) {
	record.Reply = record.Captured.text()
	for _, offer := range record.Captured.Offers {
		if _, id, ok := approval.ParseCallback(offer.Confirm); ok {
			if found, err := w.proposals(ctx, id); err == nil {
				record.Proposals = append(record.Proposals, found...)
			}
		}
	}
	for i, run := range record.Runs {
		if i == 0 {
			record.Message = run.Request.Message
		}
		record.ToolCalls = append(record.ToolCalls, run.ToolCalls...)
		record.Usage.ModelCalls += run.Result.Usage.ModelCalls
		record.Usage.PromptTokens += run.Result.Usage.PromptTokens
		record.Usage.OutputTokens += run.Result.Usage.OutputTokens
		record.Tokens.Input += run.Result.Tokens.Input
		record.Tokens.Output += run.Result.Tokens.Output
		record.Tokens.CacheRead += run.Result.Tokens.CacheRead
		record.Tokens.CacheWrite += run.Result.Tokens.CacheWrite
		record.Model = run.Result.Model
	}
}
