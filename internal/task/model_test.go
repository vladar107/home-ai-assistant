package task_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/modelcatalog"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/task"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

var (
	chatPair = agentruntime.ModelEffort{Model: "model-small", Effort: "medium"}
	taskPair = agentruntime.ModelEffort{Model: "model-strong", Effort: "high"}
)

type fixedCatalog struct {
	models []modelcatalog.Model
	err    error
}

func (c fixedCatalog) Models(context.Context, string) ([]modelcatalog.Model, error) {
	return c.models, c.err
}

func catalogWith(ids ...string) fixedCatalog {
	var models []modelcatalog.Model
	for _, id := range ids {
		models = append(models, modelcatalog.Model{ID: id, Efforts: []string{"low", "medium", "high"}})
	}
	return fixedCatalog{models: models}
}

type fixedUsage struct{ usage *chatgpt.Usage }

func (u fixedUsage) CachedUsage(string, time.Duration) (chatgpt.Usage, bool) {
	if u.usage == nil {
		return chatgpt.Usage{}, false
	}
	return *u.usage, true
}

type recordingRunner struct {
	mu        sync.Mutex
	runs      []conversation.TaskRun
	failFirst bool
}

func (r *recordingRunner) RunTask(_ context.Context, run conversation.TaskRun) (conversation.TaskReply, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, run)
	if r.failFirst && !run.Report {
		return conversation.TaskReply{}, context.DeadlineExceeded
	}
	return conversation.TaskReply{Text: "report"}, nil
}

func (r *recordingRunner) pairs() []agentruntime.ModelEffort {
	r.mu.Lock()
	defer r.mu.Unlock()
	var pairs []agentruntime.ModelEffort
	for _, run := range r.runs {
		pairs = append(pairs, agentruntime.ModelEffort{Model: run.Model, Effort: run.Effort})
	}
	return pairs
}

type sinkDeliverer struct{ conversation.Deliverer }

func (sinkDeliverer) SendReply(context.Context, int64, int64, string) error { return nil }

type modelEnv struct {
	store     *sqlite.Store
	requestID string
}

func newModelEnv(t *testing.T) *modelEnv {
	t.Helper()
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	requestID := "33333333-3333-4333-8333-333333333333"
	session := conversation.Session{ID: "11111111-1111-4111-8111-111111111111", PrincipalID: "alice", RuntimeSessionID: "22222222-2222-4222-8222-222222222222"}
	if _, err := store.EnqueueConversation(ctx, conversation.EnqueueInput{
		BotUserID: 999, TelegramUpdateID: 7, TelegramMessageID: 1007, PrincipalID: "alice", ChatID: 101, Message: "research it",
	}, session, requestID, nil); err != nil {
		t.Fatal(err)
	}
	return &modelEnv{store: store, requestID: requestID}
}

func (e *modelEnv) service(t *testing.T, config task.Config, runner task.Runner) *task.Service {
	t.Helper()
	service, err := task.NewService(config, e.store, e.store, statusMessenger{}, sinkDeliverer{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	service.SetRunner(runner)
	return service
}

func (e *modelEnv) create(t *testing.T, service *task.Service) task.Task {
	t.Helper()
	created, err := service.Create(tools.WithTurn(t.Context(), outbox.ChatTurn(e.requestID)), "alice", e.requestID, "Compare phones", "Compare three phones.")
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (e *modelEnv) awaitDone(t *testing.T, id string) task.Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		loaded, found, err := e.store.LoadTask(t.Context(), "alice", id)
		if err != nil || !found {
			t.Fatalf("load task: %v %v", found, err)
		}
		if !loaded.Open() {
			return loaded
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the task never finished")
	return task.Task{}
}

func (e *modelEnv) run(t *testing.T, service *task.Service) {
	t.Helper()
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func baseConfig() task.Config {
	return task.Config{TaskModel: taskPair, ChatModel: chatPair}
}

func TestTaskKeepsItsPairAcrossRestartAndConfigChange(t *testing.T) {
	env := newModelEnv(t)
	created := env.create(t, env.service(t, baseConfig(), &recordingRunner{}))
	if created.Model != taskPair.Model || created.Effort != taskPair.Effort || created.ModelFallback != "" {
		t.Fatalf("created = %+v", created)
	}

	runner := &recordingRunner{failFirst: true}
	changed := task.Config{
		TaskModel: agentruntime.ModelEffort{Model: "model-other", Effort: "low"},
		ChatModel: agentruntime.ModelEffort{Model: "model-other-chat", Effort: "low"},
	}
	service := env.service(t, changed, runner)
	env.run(t, service)
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	if done := env.awaitDone(t, created.ID); done.Model != taskPair.Model {
		t.Fatalf("done = %+v", done)
	}

	pairs := runner.pairs()
	if len(pairs) != 2 || pairs[0] != taskPair || pairs[1] != taskPair {
		t.Fatalf("main and report runs used %+v, want %+v twice", pairs, taskPair)
	}
}

func TestConfirmedTaskCopiesThePairWhenItIsConfirmed(t *testing.T) {
	env := newModelEnv(t)
	config := baseConfig()
	config.Models = catalogWith(taskPair.Model)
	service := env.service(t, config, &recordingRunner{})
	arguments := json.RawMessage(`{"title":"Compare phones","brief":"Compare three phones.","request_id":"` + env.requestID + `"}`)

	if _, err := service.ExecuteApproved(t.Context(), approval.Record{ID: "0123456789abcdef", PrincipalID: "alice", Tool: "task_start", Arguments: arguments}); err != nil {
		t.Fatal(err)
	}
	tasks, err := service.Tasks(t.Context(), "alice")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %+v %v", tasks, err)
	}
	if tasks[0].Model != taskPair.Model || tasks[0].Effort != taskPair.Effort || tasks[0].ModelFallback != "" {
		t.Fatalf("confirmed task = %+v", tasks[0])
	}
}

func TestTaskFallsBackWhenTheSubscriptionCannotRunTheModel(t *testing.T) {
	unavailable := &chatgpt.Usage{ModelAvailable: map[string]bool{taskPair.Model: false, chatPair.Model: true}}
	unmarked := &chatgpt.Usage{ModelAvailable: map[string]bool{chatPair.Model: true}}
	narrow := fixedCatalog{models: []modelcatalog.Model{{ID: taskPair.Model, Efforts: []string{"low", "medium"}}, {ID: chatPair.Model, Efforts: []string{"medium"}}}}
	cases := []struct {
		name     string
		models   modelcatalog.Reader
		usage    *chatgpt.Usage
		want     agentruntime.ModelEffort
		fallback string
	}{
		{"catalog lacks the model", catalogWith(chatPair.Model), nil, chatPair, task.FallbackCatalog},
		{"catalog lacks the effort", narrow, nil, chatPair, task.FallbackCatalog},
		{"usage marks it unavailable", catalogWith(taskPair.Model, chatPair.Model), unavailable, chatPair, task.FallbackUsage},
		{"unreadable catalog", fixedCatalog{err: modelcatalog.ErrUnavailable}, nil, taskPair, ""},
		{"unreadable catalog and an unavailable mark", fixedCatalog{err: modelcatalog.ErrUnavailable}, unavailable, chatPair, task.FallbackUsage},
		{"no cached usage", catalogWith(taskPair.Model), nil, taskPair, ""},
		{"model not mentioned by usage", catalogWith(taskPair.Model), unmarked, taskPair, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newModelEnv(t)
			config := baseConfig()
			config.Models, config.Usage = c.models, fixedUsage{usage: c.usage}
			service := env.service(t, config, &recordingRunner{})
			created := env.create(t, service)
			if got := (agentruntime.ModelEffort{Model: created.Model, Effort: created.Effort}); got != c.want || created.ModelFallback != c.fallback {
				t.Fatalf("created = %+v, want %+v (%q)", created, c.want, c.fallback)
			}
			stored, _, err := service.Task(t.Context(), "alice", created.ID)
			if err != nil || stored.Model != c.want.Model || stored.ModelFallback != c.fallback {
				t.Fatalf("stored = %+v %v", stored, err)
			}
			if pair, fallback := service.EffectiveModel(t.Context(), "alice"); pair != c.want || fallback != c.fallback {
				t.Fatalf("effective = %+v %q", pair, fallback)
			}
		})
	}
}

func TestTaskFromBeforeTheMigrationRunsOnItsOwnTerms(t *testing.T) {
	env := newModelEnv(t)
	started := env.create(t, env.service(t, task.Config{}, &recordingRunner{}))
	queued := env.create(t, env.service(t, task.Config{}, &recordingRunner{}))
	if started.Model != "" || queued.Model != "" {
		t.Fatalf("pre-migration rows carry %q and %q", started.Model, queued.Model)
	}
	if _, ok, err := env.store.StartTask(t.Context(), started.ID, time.Now().Add(time.Hour), time.Now()); err != nil || !ok {
		t.Fatalf("start: %v %v", ok, err)
	}

	runner := &recordingRunner{}
	config := baseConfig()
	config.Models = catalogWith(chatPair.Model)
	service := env.service(t, config, runner)
	env.run(t, service)
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	startedDone := env.awaitDone(t, started.ID)
	queuedDone := env.awaitDone(t, queued.ID)
	if startedDone.Model != chatPair.Model || startedDone.ModelFallback != "" {
		t.Fatalf("started task = %+v, want the chat default without a fallback", startedDone)
	}
	if queuedDone.Model != chatPair.Model || queuedDone.ModelFallback != task.FallbackCatalog {
		t.Fatalf("queued task = %+v, want the task default checked, falling back", queuedDone)
	}

	available := baseConfig()
	available.Models = catalogWith(taskPair.Model, chatPair.Model)
	another := env.create(t, env.service(t, task.Config{}, &recordingRunner{}))
	second := env.service(t, available, &recordingRunner{})
	env.run(t, second)
	t.Cleanup(func() { _ = second.Shutdown(context.Background()) })
	if done := env.awaitDone(t, another.ID); done.Model != taskPair.Model || done.Effort != taskPair.Effort {
		t.Fatalf("queued task with the model available = %+v", done)
	}
}
