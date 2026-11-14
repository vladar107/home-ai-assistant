package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolList       = "project_list"
	toolGet        = "project_get"
	toolCreate     = "project_create"
	toolUpdate     = "project_update"
	toolItemAdd    = "project_item_add"
	toolItemUpdate = "project_item_update"
	toolItemCheck  = "project_item_check"
	toolItemRemove = "project_item_remove"
	toolLink       = "project_link"

	// maximumAddItems is how many items one call adds, so a first checklist fits one call.
	maximumAddItems = 20
	// actTimeout lets a change made at once run as long as one made after Confirm.
	actTimeout = 45 * time.Second
)

var (
	startProjectSummary  = locale.T("Start the project %s", "Начать проект %[1]s")
	goalSummaryLine      = locale.T("Goal: %s", "Цель: %[1]s")
	itemsSummaryLine     = locale.T("Items:\n%s", "Пункты:\n%[1]s")
	changeProjectSummary = locale.T("Change the project %s:", "Изменить проект %[1]s:")
	titleChangeLine      = locale.T("title → %s", "название → %[1]s")
	goalChangeLine       = locale.T("goal → %s", "цель → %[1]s")
	statusChangeLine     = locale.T("status → %s", "статус → %[1]s")
	noGoal               = locale.T("none", "нет")
	addItemsSummary      = locale.T("Add to the project %s:\n%s", "Добавить в проект %[1]s:\n%[2]s")
	changeItemSummary    = locale.T("Change an item of the project %s:\n%s\n→ %s", "Изменить пункт проекта %[1]s:\n%[2]s\n→ %[3]s")
	checkItemSummary     = locale.T("Check off in the project %s: %s", "Отметить выполненным в проекте %[1]s: %[2]s")
	openItemSummary      = locale.T("Open again in the project %s: %s", "Вернуть в работу в проекте %[1]s: %[2]s")
	removeItemSummary    = locale.T("Remove from the project %s: %s", "Убрать из проекта %[1]s: %[2]s")
	linkSummary          = locale.T("Link the project %s to %s", "Привязать проект %[1]s: %[2]s")
	unlinkSummary        = locale.T("Remove the link of the project %s to %s", "Убрать связь проекта %[1]s: %[2]s")
	itemRow              = locale.T("- %s", "- %[1]s")
	itemRowDue           = locale.T("- %s (due %s)", "- %[1]s (срок: %[2]s)")
	itemRowNote          = locale.T("- %s: %s", "- %[1]s: %[2]s")
	itemRowDueNote       = locale.T("- %s (due %s): %s", "- %[1]s (срок: %[2]s): %[3]s")
	linkWithLabel        = locale.T("%s %s (%s)", "%[1]s %[2]s (%[3]s)")
	linkPlain            = locale.T("%s %s", "%[1]s %[2]s")
	statusActiveWord     = locale.T("active", "активен")
	statusPausedWord     = locale.T("paused", "на паузе")
	statusDoneWord       = locale.T("done", "завершён")
	statusAbandonedWord  = locale.T("abandoned", "заброшен")
	linkNotionWord       = locale.T("notion", "Notion")
	linkNASWord          = locale.T("nas", "NAS")
	linkCollectionWord   = locale.T("collection", "коллекция")
	linkIntakeWord       = locale.T("intake", "входящее")
	linkURLWord          = locale.T("url", "веб-адрес")

	changedOutcome   = locale.T("Changed.", "Изменено.")
	addedOutcome     = locale.T("Added.", "Добавлено.")
	checkedOutcome   = locale.T("Checked off.", "Отмечено выполненным.")
	reopenedOutcome  = locale.T("Opened again.", "Возвращено в работу.")
	removedOutcome   = locale.T("Removed.", "Убрано.")
	linkedOutcome    = locale.T("Linked.", "Привязано.")
	unlinkedOutcome  = locale.T("Unlinked.", "Связь убрана.")
	startedOutcome   = locale.T("Started the project (id %s).", "Проект начат (id %[1]s).")
	abandonedOutcome = locale.T("Abandoned the project %s; close its tab with /close.", "Проект %[1]s заброшен; закрой его вкладку командой /close.")
	restoredOutcome  = locale.T("Restored the project %s as it was.", "Проект %[1]s возвращён, как был.")

	malformedChanged  = locale.T("The confirmed change could not be read; nothing was changed.", "Не удалось прочитать подтверждённое изменение, ничего не изменено.")
	malformedAdded    = locale.T("The confirmed change could not be read; nothing was added.", "Не удалось прочитать подтверждённое изменение, ничего не добавлено.")
	malformedRemoved  = locale.T("The confirmed change could not be read; nothing was removed.", "Не удалось прочитать подтверждённое изменение, ничего не убрано.")
	malformedLinked   = locale.T("The confirmed change could not be read; nothing was linked.", "Не удалось прочитать подтверждённое изменение, ничего не привязано.")
	malformedCreated  = locale.T("The confirmed change could not be read; nothing was created.", "Не удалось прочитать подтверждённое изменение, ничего не создано.")
	itemGoneChanged   = locale.T("That item no longer exists; nothing was changed.", "Этого пункта больше нет, ничего не изменено.")
	itemGoneRemoved   = locale.T("That item no longer exists; nothing was removed.", "Этого пункта больше нет, ничего не убрано.")
	itemsAlreadyAdded = locale.T("Those items were already added.", "Эти пункты уже добавлены.")
	linkGone          = locale.T("That link no longer exists.", "Этой связи больше нет.")
	linkAlready       = locale.T("The project already has that link.", "У проекта уже есть эта связь.")
	linksFull         = locale.T("A project holds at most %d links; nothing was linked.", "В проекте может быть не больше %[1]d связей, ничего не привязано.")
	actionUnavailable = locale.T("That action is no longer available.", "Это действие больше недоступно.")
	projectGoneChange = locale.T("That project no longer exists; nothing was changed.", "Этого проекта больше нет, ничего не изменено.")
	changeInvalid     = locale.T("That change is not valid (%s); nothing was changed.", "Это изменение недопустимо (%[1]s), ничего не изменено.")
	projectInvalid    = locale.T("That project is not valid (%s); nothing was created.", "Этот проект недопустим (%[1]s), ничего не создано.")
	tooManyChanged    = locale.T("The user already keeps %d open projects, the most there can be; finish or abandon one with project_update first. Nothing was changed.", "У тебя уже %[1]d открытых проектов, больше нельзя: сначала заверши или заброси один. Ничего не изменено.")
	tooManyCreated    = locale.T("The user already keeps %d open projects, the most there can be; finish or abandon one with project_update first. Nothing was created.", "У тебя уже %[1]d открытых проектов, больше нельзя: сначала заверши или заброси один. Ничего не создано.")
	tooManyUndo       = locale.T("The user already keeps %d open projects, the most there can be; finish or abandon one with project_update first.", "у тебя уже %[1]d открытых проектов, больше нельзя: сначала заверши или заброси один.")
	undoUnreadable    = locale.T("what to undo could not be read.", "не удалось прочитать, что нужно отменить.")
	undoChangedSince  = locale.T("the project was changed since; change it with a new request instead.", "проект с тех пор изменился; измени его новой просьбой.")
	undoProjectGone   = locale.T("the project no longer exists.", "проекта больше нет.")
)

const guidance = "Projects: a project is a goal the user works toward over days or weeks, such as finding a dentist, renovating a room, or preparing a tax return, " +
	"with a checklist of items, each with an optional due date and note, and links to related things: Notion pages, NAS paths, collections, intake records, and web addresses. " +
	"Use a trip for travel and a project for anything else; never turn a trip into a project. " +
	"project_list lists the user's projects with their ids; project_get shows one with its item ids, links, and the jobs routed to it. " +
	"A conversation in a project's tab is about that project, and its turns name the project's open items with their ids."

func writeGuidance() string {
	return "project_create starts a project when the user asks for one, with its title, goal, and first items; with tabs on it gets its own tab with a pinned summary that stays current. " +
		"project_update changes the title, goal, or status (active, paused, done, or abandoned). project_item_add, project_item_update, project_item_check, and project_item_remove change the checklist by item id, and project_link adds or removes a link. " +
		"When the user asked for it in this conversation and the conversation has read no outside content, each is done at once and the user can undo the changes; tell the user it is done. After outside content it is only proposed with Confirm and Reject buttons, so never claim it is saved. " +
		"Never invent an item, a due date, or a link the user did not give. " +
		"A nudge on a cadence or a reminder for an item is an ordinary schedule_propose job or reminder_set with project_id, so it arrives in the project's tab and its run reads the project's open items; set one only when the user asks. " +
		"schedule_update with project_id moves a job to a project, and with project_id none takes it off its project. " +
		"When a project is done or abandoned, offer to delete the jobs still routed to it. " +
		fmt.Sprintf("Each user may keep %d open projects of up to %d items and %d links each.", MaximumOpenProjects, MaximumItems, MaximumLinks)
}

const itemSchema = `{"type":"object","properties":{` +
	`"text":{"type":"string","maxLength":300,"description":"What is to be done, such as call the clinic."},` +
	`"due_on":{"type":"string","description":"A local date, YYYY-MM-DD, by which it is due."},` +
	`"note":{"type":"string","maxLength":500}},` +
	`"required":["text"],"additionalProperties":false}`

const projectIDProperty = `"project_id":{"type":"string","description":"The project's id from project_list."}`

var readDefinitions = []tools.Definition{
	{
		Name:        toolList,
		Description: "List the user's projects with their ids, status, open items, and next due date.",
		Risk:        tools.RiskRead,
		Private:     true,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"include_closed":{"type":"boolean","description":"Also list done and abandoned projects."}},` +
			`"additionalProperties":false}`),
	},
	{
		Name:        toolGet,
		Description: "Show one project in full: goal, status, every item with its id, due date, and note, its links, and the jobs routed to it.",
		Risk:        tools.RiskRead,
		SourceLabel: tools.ArgumentLabel("project_id"),
		Private:     true,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + projectIDProperty + `},"required":["project_id"],"additionalProperties":false}`),
	},
}

var createDefinition = tools.Definition{
	Name:        toolCreate,
	Description: "Start a project with a title, an optional goal, and first items. Done at once and undoable, or proposed for Confirm after outside content.",
	Risk:        tools.RiskWrite,
	SourceLabel: tools.ArgumentLabel("title"),
	Timeout:     actTimeout,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"title":{"type":"string","maxLength":100,"description":"A short title, such as Find a dentist."},` +
		`"goal":{"type":"string","maxLength":500,"description":"What done looks like, in a sentence or two."},` +
		`"items":{"type":"array","maxItems":20,"items":` + itemSchema + `}},` +
		`"required":["title"],"additionalProperties":false}`),
}

var writeDefinitions = []tools.Definition{
	{
		Name:        toolUpdate,
		Description: "Change a project's title, goal, or status; omitted fields keep their values. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		SourceLabel: tools.ArgumentLabel("project_id"),
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + projectIDProperty + `,` +
			`"title":{"type":"string","maxLength":100},` +
			`"goal":{"type":"string","maxLength":500,"description":"The new goal; an empty string clears it."},` +
			`"status":{"type":"string","enum":["active","paused","done","abandoned"]}},` +
			`"required":["project_id"],"additionalProperties":false}`),
	},
	{
		Name:        toolItemAdd,
		Description: "Add items to a project's checklist. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		SourceLabel: tools.ArgumentLabel("project_id"),
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + projectIDProperty + `,` +
			`"items":{"type":"array","minItems":1,"maxItems":20,"items":` + itemSchema + `}},` +
			`"required":["project_id","items"],"additionalProperties":false}`),
	},
	{
		Name:        toolItemUpdate,
		Description: "Change one item's text, due date, or note; omitted fields keep their values. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		SourceLabel: tools.ArgumentLabel("project_id", "item_id"),
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + projectIDProperty + `,` +
			`"item_id":{"type":"string","description":"The item's id from project_get or the turn's project line."},` +
			`"text":{"type":"string","maxLength":300},` +
			`"due_on":{"type":"string","description":"A local date, YYYY-MM-DD; an empty string clears it."},` +
			`"note":{"type":"string","maxLength":500,"description":"The new note; an empty string clears it."}},` +
			`"required":["project_id","item_id"],"additionalProperties":false}`),
	},
	{
		Name:        toolItemCheck,
		Description: "Check an item off, or open it again with done false. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		SourceLabel: tools.ArgumentLabel("project_id", "item_id"),
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + projectIDProperty + `,` +
			`"item_id":{"type":"string","description":"The item's id from project_get or the turn's project line."},` +
			`"done":{"type":"boolean","description":"false opens a checked item again; default true."}},` +
			`"required":["project_id","item_id"],"additionalProperties":false}`),
	},
	{
		Name:        toolItemRemove,
		Description: "Remove one item from a project's checklist. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		SourceLabel: tools.ArgumentLabel("project_id", "item_id"),
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + projectIDProperty + `,` +
			`"item_id":{"type":"string","description":"The item's id from project_get or the turn's project line."}},` +
			`"required":["project_id","item_id"],"additionalProperties":false}`),
	},
	{
		Name:        toolLink,
		Description: "Link a project to a related thing, or remove a link with remove true. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		SourceLabel: tools.ArgumentLabel("project_id", "kind"),
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + projectIDProperty + `,` +
			`"kind":{"type":"string","enum":["notion","nas","collection","intake","url"]},` +
			`"reference":{"type":"string","maxLength":500,"description":"A Notion page's id or address, a NAS path, a collection's name, an intake record's id, or a web address."},` +
			`"label":{"type":"string","maxLength":100,"description":"A short label for the link."},` +
			`"remove":{"type":"boolean","description":"true removes the link with this kind and reference."}},` +
			`"required":["project_id","kind","reference"],"additionalProperties":false}`),
	},
}

var (
	_ tools.Provider    = (*Service)(nil)
	_ approval.Executor = (*Service)(nil)
	_ approval.Undoer   = (*Service)(nil)
)

// Contribute offers project_create whenever changes can be proposed, and the other tools only while
// the principal keeps a project, so a user without any pays for one tool alone.
func (s *Service) Contribute(ctx context.Context, principalID string) (tools.Contribution, error) {
	total, _, err := s.repository.CountProjects(ctx, principalID)
	if err != nil {
		return tools.Contribution{}, err
	}
	var contribution tools.Contribution
	if total > 0 {
		contribution.Guidance = guidance
		contribution.Definitions = slices.Clone(readDefinitions)
	}
	if s.proposer != nil {
		contribution.WriteGuidance = writeGuidance()
		if total == 0 {
			contribution.WriteGuidance = guidance + "\n" + contribution.WriteGuidance
		}
		contribution.Definitions = append(contribution.Definitions, createDefinition)
		if total > 0 {
			contribution.Definitions = append(contribution.Definitions, writeDefinitions...)
		}
	}
	return contribution, nil
}

func (s *Service) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	switch call.Name {
	case toolList:
		return s.list(ctx, principalID, call)
	case toolGet:
		return s.get(ctx, principalID, call)
	case toolCreate:
		return s.proposeCreate(ctx, principalID, call)
	case toolUpdate:
		return s.proposeUpdate(ctx, principalID, call)
	case toolItemAdd:
		return s.proposeItemAdd(ctx, principalID, call)
	case toolItemUpdate:
		return s.proposeItemUpdate(ctx, principalID, call)
	case toolItemCheck:
		return s.proposeItemCheck(ctx, principalID, call)
	case toolItemRemove:
		return s.proposeItemRemove(ctx, principalID, call)
	case toolLink:
		return s.proposeLink(ctx, principalID, call)
	default:
		return errorResult("unknown project tool"), nil
	}
}

type listArguments struct {
	IncludeClosed bool `json:"include_closed"`
}

type projectArguments struct {
	ProjectID string `json:"project_id"`
}

type createArguments struct {
	Title string `json:"title"`
	Goal  string `json:"goal"`
	Items []Item `json:"items"`
}

type updateArguments struct {
	ProjectID string  `json:"project_id"`
	Title     *string `json:"title"`
	Goal      *string `json:"goal"`
	Status    *string `json:"status"`
}

type itemAddArguments struct {
	ProjectID string `json:"project_id"`
	Items     []Item `json:"items"`
}

type itemUpdateArguments struct {
	ProjectID string  `json:"project_id"`
	ItemID    string  `json:"item_id"`
	Text      *string `json:"text"`
	DueOn     *string `json:"due_on"`
	Note      *string `json:"note"`
}

type itemCheckArguments struct {
	ProjectID string `json:"project_id"`
	ItemID    string `json:"item_id"`
	Done      *bool  `json:"done"`
}

type itemRemoveArguments struct {
	ProjectID string `json:"project_id"`
	ItemID    string `json:"item_id"`
}

type linkArguments struct {
	ProjectID string `json:"project_id"`
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Label     string `json:"label"`
	Remove    bool   `json:"remove"`
}

// The confirmed forms are what the user confirms and the executor applies to the project as it is
// then. Items carry no IDs; the approval's own ID names them.
type confirmedCreate struct {
	Title string `json:"title"`
	Goal  string `json:"goal,omitempty"`
	Items []Item `json:"items,omitempty"`
}

type confirmedUpdate struct {
	ProjectID string  `json:"project_id"`
	Title     *string `json:"title,omitempty"`
	Goal      *string `json:"goal,omitempty"`
	Status    *string `json:"status,omitempty"`
}

type confirmedItemAdd struct {
	ProjectID string `json:"project_id"`
	Items     []Item `json:"items"`
}

type confirmedItemUpdate struct {
	ProjectID string  `json:"project_id"`
	ItemID    string  `json:"item_id"`
	Text      *string `json:"text,omitempty"`
	DueOn     *string `json:"due_on,omitempty"`
	Note      *string `json:"note,omitempty"`
}

type confirmedItemCheck struct {
	ProjectID string `json:"project_id"`
	ItemID    string `json:"item_id"`
	Done      bool   `json:"done"`
}

type confirmedItemRemove struct {
	ProjectID string `json:"project_id"`
	ItemID    string `json:"item_id"`
}

type confirmedLink struct {
	ProjectID string `json:"project_id"`
	Link      Link   `json:"link"`
	Remove    bool   `json:"remove,omitempty"`
}

// projectUndo puts back the project as a direct change found it, while it is as the change left it.
type projectUndo struct {
	ProjectID string    `json:"project_id"`
	UpdatedAt time.Time `json:"updated_at"`
	Title     string    `json:"title"`
	Goal      string    `json:"goal"`
	Status    string    `json:"status"`
	Items     []Item    `json:"items"`
	Links     []Link    `json:"links"`
}

func (s *Service) list(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments listArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	projects, err := s.repository.ListProjects(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	var lines []string
	closed := 0
	for _, found := range projects {
		if !found.Open() && !arguments.IncludeClosed {
			closed++
			continue
		}
		lines = append(lines, "- "+listLine(found)+" (id "+found.ID+")")
	}
	if len(lines) == 0 {
		lines = append(lines, "The user has no open projects.")
	}
	if closed > 0 {
		lines = append(lines, fmt.Sprintf("%d done or abandoned projects are left out; include_closed lists them.", closed))
	}
	return tools.Result{Text: strings.Join(lines, "\n")}, nil
}

// listLine is a project in one line, as project_list shows it.
func listLine(found Project) string {
	line := strconv.Quote(found.Title) + ": " + found.Status
	open := found.OpenItems()
	if len(found.Items) > 0 {
		line += fmt.Sprintf(", %d of %d items open", len(open), len(found.Items))
	}
	if next, due := found.NextDue(); due {
		line += ", next due " + next.DueOn
	}
	return line
}

func (s *Service) get(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments projectArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	found, refusal, err := s.named(ctx, principalID, arguments.ProjectID)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	lines := []string{"Project " + strconv.Quote(found.Title) + " (id " + found.ID + "), " + found.Status}
	if found.Goal != "" {
		lines = append(lines, "Goal: "+strconv.Quote(found.Goal))
	}
	if len(found.Items) == 0 {
		lines = append(lines, "No items.")
	} else {
		lines = append(lines, "Items:")
	}
	for _, item := range found.Items {
		line := "- [" + item.ID + "] "
		if item.Done() {
			line += "done: "
		}
		line += strconv.Quote(item.Text)
		if item.DueOn != "" {
			line += ", due " + item.DueOn
		}
		if item.Note != "" {
			line += ", note " + strconv.Quote(item.Note)
		}
		lines = append(lines, line)
	}
	if len(found.Links) > 0 {
		lines = append(lines, "Links:")
	}
	for _, link := range found.Links {
		line := "- " + link.Kind + ": " + strconv.Quote(link.Reference)
		if link.Label != "" {
			line += " (" + strconv.Quote(link.Label) + ")"
		}
		lines = append(lines, line)
	}
	jobs, err := s.pendingJobs(ctx, principalID, found.ID)
	if err != nil {
		return tools.Result{}, err
	}
	if len(jobs) > 0 {
		lines = append(lines, "Jobs routed to it: "+describeJobs(jobs)+".")
	}
	return tools.Result{Text: strings.Join(lines, "\n")}, nil
}

func describeJobs(jobs []schedule.Job) string {
	names := make([]string, 0, len(jobs))
	for _, job := range jobs {
		names = append(names, strconv.Quote(job.Name)+" (id "+job.ID+")")
	}
	return strings.Join(names, ", ")
}

// named finds the principal's project by its ID; a refusal is text for the model.
func (s *Service) named(ctx context.Context, principalID, projectID string) (Project, string, error) {
	id := strings.TrimSpace(projectID)
	found, known, err := s.repository.LoadProject(ctx, principalID, id)
	if err != nil {
		return Project{}, "", err
	}
	if !known {
		return Project{}, fmt.Sprintf("No project has the id %q; find it with project_list.", id), nil
	}
	return found, "", nil
}

func (s *Service) proposeCreate(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Projects cannot be created here."), nil
	}
	var arguments createArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	confirmed, err := normalizeCreate(confirmedCreate(arguments))
	if err != nil {
		return errorResult(err.Error()), nil
	}
	_, open, err := s.repository.CountProjects(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	if open >= MaximumOpenProjects {
		return errorResult(tooManyText()), nil
	}
	canonical, err := json.Marshal(confirmed)
	if err != nil {
		return tools.Result{}, err
	}
	summary := startProjectSummary.Format(ctx, confirmed.Title)
	if confirmed.Goal != "" {
		summary += "\n" + goalSummaryLine.Format(ctx, confirmed.Goal)
	}
	if len(confirmed.Items) > 0 {
		summary += "\n" + itemsSummaryLine.Format(ctx, describeItems(ctx, confirmed.Items))
	}
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolCreate, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "start", TargetKind: "project", TargetName: confirmed.Title}})
}

func normalizeCreate(confirmed confirmedCreate) (confirmedCreate, error) {
	title, err := normalizeTitle(confirmed.Title)
	if err != nil {
		return confirmedCreate{}, err
	}
	goal, err := normalizeGoal(confirmed.Goal)
	if err != nil {
		return confirmedCreate{}, err
	}
	items, err := normalizeNewItems(confirmed.Items, 0)
	if err != nil {
		return confirmedCreate{}, err
	}
	return confirmedCreate{Title: title, Goal: goal, Items: items}, nil
}

// normalizeNewItems checks items to add to a project that holds existing ones; their IDs and done
// times are dropped.
func normalizeNewItems(items []Item, existing int) ([]Item, error) {
	if len(items) > maximumAddItems {
		return nil, fmt.Errorf("give at most %d items at once", maximumAddItems)
	}
	if existing+len(items) > MaximumItems {
		return nil, fmt.Errorf("a project holds at most %d items; remove done ones first", MaximumItems)
	}
	normalized := make([]Item, 0, len(items))
	for index, item := range items {
		item.ID, item.DoneAt = "", time.Time{}
		checked, err := normalizeItem(item)
		if err != nil {
			if len(items) > 1 {
				return nil, fmt.Errorf("item %d: %w", index+1, err)
			}
			return nil, err
		}
		normalized = append(normalized, checked)
	}
	return normalized, nil
}

func describeItems(ctx context.Context, items []Item) string {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		switch {
		case item.DueOn != "" && item.Note != "":
			lines = append(lines, itemRowDueNote.Format(ctx, item.Text, item.DueOn, item.Note))
		case item.DueOn != "":
			lines = append(lines, itemRowDue.Format(ctx, item.Text, item.DueOn))
		case item.Note != "":
			lines = append(lines, itemRowNote.Format(ctx, item.Text, item.Note))
		default:
			lines = append(lines, itemRow.Format(ctx, item.Text))
		}
	}
	return strings.Join(lines, "\n")
}

func tooManyText() string {
	return fmt.Sprintf("The user already keeps %d open projects, the most there can be; finish or abandon one with project_update first.", MaximumOpenProjects)
}

func (s *Service) proposeUpdate(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Projects cannot be changed here."), nil
	}
	var arguments updateArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	found, refusal, err := s.named(ctx, principalID, arguments.ProjectID)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	confirmed := confirmedUpdate{ProjectID: found.ID, Title: arguments.Title, Goal: arguments.Goal, Status: arguments.Status}
	next := found.clone()
	if err := applyUpdate(&next, confirmed); err != nil {
		return errorResult(err.Error()), nil
	}
	if next.Title == found.Title && next.Goal == found.Goal && next.Status == found.Status {
		return errorResult("That changes nothing; the project already reads exactly like this."), nil
	}
	canonical, err := json.Marshal(confirmed)
	if err != nil {
		return tools.Result{}, err
	}
	var changes []string
	if next.Title != found.Title {
		changes = append(changes, titleChangeLine.Format(ctx, next.Title))
	}
	if next.Goal != found.Goal {
		goal := next.Goal
		if goal == "" {
			goal = noGoal.Format(ctx)
		}
		changes = append(changes, goalChangeLine.Format(ctx, goal))
	}
	if next.Status != found.Status {
		changes = append(changes, statusChangeLine.Format(ctx, statusWord(ctx, next.Status)))
	}
	summary := changeProjectSummary.Format(ctx, found.Title) + "\n- " + strings.Join(changes, "\n- ")
	result, err := s.act(ctx, principalID, call, approval.Proposal{Tool: toolUpdate, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "change", TargetKind: "project", TargetName: found.Title, NewName: next.Title}})
	if err != nil || result.IsError || next.Open() {
		return result, err
	}
	jobs, err := s.pendingJobs(ctx, principalID, found.ID)
	if err != nil {
		return tools.Result{}, err
	}
	if len(jobs) > 0 {
		result.Text += " Jobs still routed to this project: " + describeJobs(jobs) + ". Ask the user whether to delete them; delete each they agree to with schedule_remove, or reminder_cancel for a reminder."
	}
	return result, nil
}

func statusWord(ctx context.Context, status string) string {
	switch status {
	case StatusActive:
		return statusActiveWord.Format(ctx)
	case StatusPaused:
		return statusPausedWord.Format(ctx)
	case StatusDone:
		return statusDoneWord.Format(ctx)
	case StatusAbandoned:
		return statusAbandonedWord.Format(ctx)
	}
	return status
}

func cmpText(value, empty string) string {
	if value == "" {
		return empty
	}
	return value
}

// applyUpdate changes the fields an update names.
func applyUpdate(next *Project, confirmed confirmedUpdate) error {
	if confirmed.Title != nil {
		title, err := normalizeTitle(*confirmed.Title)
		if err != nil {
			return err
		}
		next.Title = title
	}
	if confirmed.Goal != nil {
		goal, err := normalizeGoal(*confirmed.Goal)
		if err != nil {
			return err
		}
		next.Goal = goal
	}
	if confirmed.Status != nil {
		status, err := normalizeStatus(*confirmed.Status)
		if err != nil {
			return err
		}
		next.Status = status
	}
	return nil
}

func (s *Service) proposeItemAdd(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Projects cannot be changed here."), nil
	}
	var arguments itemAddArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	found, refusal, err := s.named(ctx, principalID, arguments.ProjectID)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	if len(arguments.Items) == 0 {
		return errorResult("give at least one item"), nil
	}
	items, err := normalizeNewItems(arguments.Items, len(found.Items))
	if err != nil {
		return errorResult(err.Error()), nil
	}
	canonical, err := json.Marshal(confirmedItemAdd{ProjectID: found.ID, Items: items})
	if err != nil {
		return tools.Result{}, err
	}
	summary := addItemsSummary.Format(ctx, found.Title, describeItems(ctx, items))
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolItemAdd, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "add items to", TargetKind: "project", TargetName: found.Title}})
}

// namedItem finds the project and one of its items; a refusal is text for the model.
func (s *Service) namedItem(ctx context.Context, principalID, projectID, itemID string) (Project, Item, string, error) {
	found, refusal, err := s.named(ctx, principalID, projectID)
	if err != nil || refusal != "" {
		return Project{}, Item{}, refusal, err
	}
	index, known := found.item(strings.TrimSpace(itemID))
	if !known {
		return Project{}, Item{}, fmt.Sprintf("The project %s has no item with id %q; find it with project_get.", found.Title, strings.TrimSpace(itemID)), nil
	}
	return found, found.Items[index], "", nil
}

func (s *Service) proposeItemUpdate(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Projects cannot be changed here."), nil
	}
	var arguments itemUpdateArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	found, item, refusal, err := s.namedItem(ctx, principalID, arguments.ProjectID, arguments.ItemID)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	confirmed := confirmedItemUpdate{ProjectID: found.ID, ItemID: item.ID, Text: arguments.Text, DueOn: arguments.DueOn, Note: arguments.Note}
	changed, err := applyItemUpdate(item, confirmed)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	if changed == item {
		return errorResult("That changes nothing; the item already reads exactly like this."), nil
	}
	canonical, err := json.Marshal(confirmed)
	if err != nil {
		return tools.Result{}, err
	}
	summary := changeItemSummary.Format(ctx, found.Title, describeItems(ctx, []Item{item}), strings.TrimPrefix(describeItems(ctx, []Item{changed}), "- "))
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolItemUpdate, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "change an item of", TargetKind: "project", TargetName: found.Title}})
}

func applyItemUpdate(item Item, confirmed confirmedItemUpdate) (Item, error) {
	if confirmed.Text != nil {
		item.Text = *confirmed.Text
	}
	if confirmed.DueOn != nil {
		item.DueOn = *confirmed.DueOn
	}
	if confirmed.Note != nil {
		item.Note = *confirmed.Note
	}
	return normalizeItem(item)
}

func (s *Service) proposeItemCheck(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Projects cannot be changed here."), nil
	}
	var arguments itemCheckArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	found, item, refusal, err := s.namedItem(ctx, principalID, arguments.ProjectID, arguments.ItemID)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	done := arguments.Done == nil || *arguments.Done
	if item.Done() == done {
		if done {
			return errorResult("That item is already checked off."), nil
		}
		return errorResult("That item is already open."), nil
	}
	canonical, err := json.Marshal(confirmedItemCheck{ProjectID: found.ID, ItemID: item.ID, Done: done})
	if err != nil {
		return tools.Result{}, err
	}
	verb, summary := "check off an item of", checkItemSummary.Format(ctx, found.Title, item.Text)
	if !done {
		verb, summary = "open an item of", openItemSummary.Format(ctx, found.Title, item.Text)
	}
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolItemCheck, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: verb, TargetKind: "project", TargetName: found.Title}})
}

func (s *Service) proposeItemRemove(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Projects cannot be changed here."), nil
	}
	var arguments itemRemoveArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	found, item, refusal, err := s.namedItem(ctx, principalID, arguments.ProjectID, arguments.ItemID)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	canonical, err := json.Marshal(confirmedItemRemove{ProjectID: found.ID, ItemID: item.ID})
	if err != nil {
		return tools.Result{}, err
	}
	summary := removeItemSummary.Format(ctx, found.Title, item.Text)
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolItemRemove, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "remove an item from", TargetKind: "project", TargetName: found.Title}})
}

func (s *Service) proposeLink(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Projects cannot be changed here."), nil
	}
	var arguments linkArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	found, refusal, err := s.named(ctx, principalID, arguments.ProjectID)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	link, err := normalizeLink(Link{Kind: arguments.Kind, Reference: arguments.Reference, Label: arguments.Label})
	if err != nil {
		return errorResult(err.Error()), nil
	}
	if arguments.Remove {
		index := slices.IndexFunc(found.Links, func(existing Link) bool { return existing.Kind == link.Kind && existing.Reference == link.Reference })
		if index < 0 && link.Kind == LinkCollection {
			index = slices.IndexFunc(found.Links, func(existing Link) bool {
				return existing.Kind == LinkCollection && strings.EqualFold(existing.Label, link.Reference)
			})
		}
		if index < 0 {
			return errorResult(fmt.Sprintf("The project %s has no %s link to %q; project_get lists its links.", found.Title, link.Kind, link.Reference)), nil
		}
		link = found.Links[index]
	} else {
		if link.Kind == LinkCollection {
			if link, refusal, err = s.collectionLink(ctx, principalID, link); err != nil {
				return tools.Result{}, err
			}
			if refusal != "" {
				return errorResult(refusal), nil
			}
		}
		if slices.ContainsFunc(found.Links, func(existing Link) bool { return existing.Kind == link.Kind && existing.Reference == link.Reference }) {
			return errorResult("The project already has that link."), nil
		}
		if len(found.Links) >= MaximumLinks {
			return errorResult(fmt.Sprintf("A project holds at most %d links; remove one first.", MaximumLinks)), nil
		}
	}
	canonical, err := json.Marshal(confirmedLink{ProjectID: found.ID, Link: link, Remove: arguments.Remove})
	if err != nil {
		return tools.Result{}, err
	}
	summary, verb := linkSummary.Format(ctx, found.Title, describeLink(ctx, link)), "link"
	if arguments.Remove {
		summary, verb = unlinkSummary.Format(ctx, found.Title, describeLink(ctx, link)), "unlink"
	}
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolLink, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: verb, TargetKind: "project", TargetName: found.Title}})
}

// collectionLink resolves a collection named by its name or ID to a link to its ID, labelled with
// its name; a refusal is text for the model.
func (s *Service) collectionLink(ctx context.Context, principalID string, link Link) (Link, string, error) {
	if s.collections == nil {
		return Link{}, "Collections cannot be linked here.", nil
	}
	collections, err := s.collections.ListCollections(ctx, principalID)
	if err != nil {
		return Link{}, "", err
	}
	for _, found := range collections {
		if found.ID == link.Reference || strings.EqualFold(found.Name, link.Reference) {
			return Link{Kind: LinkCollection, Reference: found.ID, Label: cmpText(link.Label, found.Name)}, "", nil
		}
	}
	return Link{}, fmt.Sprintf("The user has no collection called %q.", link.Reference), nil
}

func describeLink(ctx context.Context, link Link) string {
	kind := link.Kind
	switch link.Kind {
	case LinkNotion:
		kind = linkNotionWord.Format(ctx)
	case LinkNAS:
		kind = linkNASWord.Format(ctx)
	case LinkCollection:
		kind = linkCollectionWord.Format(ctx)
	case LinkIntake:
		kind = linkIntakeWord.Format(ctx)
	case LinkURL:
		kind = linkURLWord.Format(ctx)
	}
	if link.Label != "" {
		return linkWithLabel.Format(ctx, kind, link.Label, link.Reference)
	}
	return linkPlain.Format(ctx, kind, link.Reference)
}

// act makes the change at once in a clean turn and proposes it otherwise.
func (s *Service) act(ctx context.Context, principalID string, call tools.Call, proposal approval.Proposal) (tools.Result, error) {
	text, err := s.proposer.Act(ctx, principalID, proposal)
	if err != nil {
		if approval.Refused(err) {
			return errorResult(err.Error()), nil
		}
		s.logger.WarnContext(ctx, "project.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return errorResult("the change could not be recorded"), nil
	}
	return tools.Result{Text: text}, nil
}

func (s *Service) ActionTools() []string {
	return []string{toolCreate, toolUpdate, toolItemAdd, toolItemUpdate, toolItemCheck, toolItemRemove, toolLink}
}

func projectFailure(ctx context.Context, reason string, text locale.Text) *approval.ActionError {
	return &approval.ActionError{Reason: reason, Text: text.In(locale.English), UserText: text.Format(ctx)}
}

func projectRefusal(ctx context.Context, reason locale.Text) *approval.UndoRefusal {
	return &approval.UndoRefusal{Reason: reason.In(locale.English), UserReason: reason.Format(ctx)}
}

func malformed(ctx context.Context, text locale.Text) error {
	return projectFailure(ctx, "malformed", text)
}

// ExecuteApproved makes the confirmed change to the project as it is now. Everything is checked
// again, and the approval's own ID names what it creates, so a second execution cannot create
// anything twice.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	if record.Tool == toolCreate {
		return s.executeCreate(ctx, record)
	}
	var projectID string
	var apply func(*Project) error
	text := changedOutcome
	switch record.Tool {
	case toolUpdate:
		var confirmed confirmedUpdate
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, malformed(ctx, malformedChanged)
		}
		projectID = confirmed.ProjectID
		apply = func(next *Project) error { return invalid(ctx, applyUpdate(next, confirmed)) }
	case toolItemAdd:
		var confirmed confirmedItemAdd
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, malformed(ctx, malformedAdded)
		}
		projectID, text = confirmed.ProjectID, addedOutcome
		apply = func(next *Project) error {
			items, err := normalizeNewItems(confirmed.Items, len(next.Items))
			if err != nil {
				return invalid(ctx, err)
			}
			for index, item := range items {
				item.ID = itemID(record.ID, index)
				if _, taken := next.item(item.ID); taken {
					return projectFailure(ctx, "duplicate", itemsAlreadyAdded)
				}
				next.Items = append(next.Items, item)
			}
			return nil
		}
	case toolItemUpdate:
		var confirmed confirmedItemUpdate
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, malformed(ctx, malformedChanged)
		}
		projectID = confirmed.ProjectID
		apply = func(next *Project) error {
			index, known := next.item(confirmed.ItemID)
			if !known {
				return itemGone(ctx, itemGoneChanged)
			}
			changed, err := applyItemUpdate(next.Items[index], confirmed)
			if err != nil {
				return invalid(ctx, err)
			}
			next.Items[index] = changed
			return nil
		}
	case toolItemCheck:
		var confirmed confirmedItemCheck
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, malformed(ctx, malformedChanged)
		}
		projectID, text = confirmed.ProjectID, checkedOutcome
		if !confirmed.Done {
			text = reopenedOutcome
		}
		apply = func(next *Project) error {
			index, known := next.item(confirmed.ItemID)
			if !known {
				return itemGone(ctx, itemGoneChanged)
			}
			switch {
			case confirmed.Done && !next.Items[index].Done():
				next.Items[index].DoneAt = s.stamp()
			case !confirmed.Done:
				next.Items[index].DoneAt = time.Time{}
			}
			return nil
		}
	case toolItemRemove:
		var confirmed confirmedItemRemove
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, malformed(ctx, malformedRemoved)
		}
		projectID, text = confirmed.ProjectID, removedOutcome
		apply = func(next *Project) error {
			index, known := next.item(confirmed.ItemID)
			if !known {
				return itemGone(ctx, itemGoneRemoved)
			}
			next.Items = slices.Delete(next.Items, index, index+1)
			return nil
		}
	case toolLink:
		var confirmed confirmedLink
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, malformed(ctx, malformedLinked)
		}
		link, err := normalizeLink(confirmed.Link)
		if err != nil {
			return approval.Outcome{}, invalid(ctx, err)
		}
		projectID, text = confirmed.ProjectID, linkedOutcome
		if confirmed.Remove {
			text = unlinkedOutcome
		}
		apply = func(next *Project) error {
			index := slices.IndexFunc(next.Links, func(existing Link) bool { return existing.Kind == link.Kind && existing.Reference == link.Reference })
			switch {
			case confirmed.Remove && index < 0:
				return projectFailure(ctx, "not_found", linkGone)
			case confirmed.Remove:
				next.Links = slices.Delete(next.Links, index, index+1)
			case index >= 0:
				return projectFailure(ctx, "duplicate", linkAlready)
			case len(next.Links) >= MaximumLinks:
				return &approval.ActionError{Reason: "too_many", Text: linksFull.FormatIn(locale.English, MaximumLinks), UserText: linksFull.Format(ctx, MaximumLinks)}
			default:
				next.Links = append(next.Links, link)
			}
			return nil
		}
	default:
		return approval.Outcome{}, projectFailure(ctx, "unavailable", actionUnavailable)
	}
	before, after, found, err := s.change(ctx, record.PrincipalID, projectID, apply)
	switch {
	case errors.Is(err, ErrTooManyProjects):
		return approval.Outcome{}, &approval.ActionError{Reason: "too_many", Text: tooManyText() + " Nothing was changed.", UserText: tooManyChanged.Format(ctx, MaximumOpenProjects)}
	case err != nil:
		return approval.Outcome{}, err
	case !found:
		return approval.Outcome{}, projectFailure(ctx, "not_found", projectGoneChange)
	}
	s.logger.InfoContext(ctx, "project.changed", "principal_id", record.PrincipalID, "project_id", projectID, "tool", record.Tool, "items", len(after.Items), "links", len(after.Links))
	s.changed(ctx, after)
	undo, err := json.Marshal(projectUndo{
		ProjectID: projectID, UpdatedAt: after.UpdatedAt, Title: before.Title, Goal: before.Goal, Status: before.Status, Items: before.Items, Links: before.Links,
	})
	if err != nil {
		return approval.Outcome{}, err
	}
	return approval.Outcome{Text: text.In(locale.English), UserText: text.Format(ctx), Undo: undo}, nil
}

func invalid(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	return &approval.ActionError{Reason: "invalid", Text: "That change is not valid (" + err.Error() + "); nothing was changed.", UserText: changeInvalid.Format(ctx, err.Error())}
}

func itemGone(ctx context.Context, text locale.Text) error {
	return projectFailure(ctx, "not_found", text)
}

func (s *Service) executeCreate(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed confirmedCreate
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, malformed(ctx, malformedCreated)
	}
	confirmed, err := normalizeCreate(confirmed)
	if err != nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "That project is not valid (" + err.Error() + "); nothing was created.", UserText: projectInvalid.Format(ctx, err.Error())}
	}
	now := s.stamp()
	created := Project{
		ID: record.ID, PrincipalID: record.PrincipalID, Title: confirmed.Title, Goal: confirmed.Goal, Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}
	for index, item := range confirmed.Items {
		item.ID = itemID(record.ID, index)
		created.Items = append(created.Items, item)
	}
	stored, err := s.repository.CreateProject(ctx, created, MaximumOpenProjects)
	switch {
	case errors.Is(err, ErrTooManyProjects):
		return approval.Outcome{}, &approval.ActionError{Reason: "too_many", Text: tooManyText() + " Nothing was created.", UserText: tooManyCreated.Format(ctx, MaximumOpenProjects)}
	case err != nil:
		return approval.Outcome{}, err
	}
	if stored {
		s.logger.InfoContext(ctx, "project.created", "principal_id", record.PrincipalID, "project_id", record.ID, "items", len(created.Items))
		s.changed(ctx, created)
	}
	undo, err := json.Marshal(projectUndo{ProjectID: record.ID, UpdatedAt: created.UpdatedAt})
	if err != nil {
		return approval.Outcome{}, err
	}
	return approval.Outcome{Text: startedOutcome.FormatIn(locale.English, record.ID), UserText: startedOutcome.Format(ctx, record.ID), Undo: undo}, nil
}

// Undo puts the project back as a direct change found it; undoing a start abandons the project,
// so its tab and summary say so. A project changed since is refused.
func (s *Service) Undo(ctx context.Context, record approval.Record, raw json.RawMessage) (approval.Outcome, error) {
	var undo projectUndo
	if err := json.Unmarshal(raw, &undo); err != nil || undo.ProjectID == "" || !slices.Contains(s.ActionTools(), record.Tool) {
		return approval.Outcome{}, projectRefusal(ctx, undoUnreadable)
	}
	refusal := projectRefusal(ctx, undoChangedSince)
	_, after, found, err := s.change(ctx, record.PrincipalID, undo.ProjectID, func(next *Project) error {
		if !next.UpdatedAt.Equal(undo.UpdatedAt) {
			return refusal
		}
		if record.Tool == toolCreate {
			next.Status = StatusAbandoned
			return nil
		}
		next.Title, next.Goal, next.Status = undo.Title, undo.Goal, undo.Status
		next.Items, next.Links = slices.Clone(undo.Items), slices.Clone(undo.Links)
		return nil
	})
	switch {
	case errors.Is(err, ErrTooManyProjects):
		return approval.Outcome{}, &approval.UndoRefusal{Reason: tooManyText(), UserReason: tooManyUndo.Format(ctx, MaximumOpenProjects)}
	case err != nil:
		return approval.Outcome{}, err
	case !found:
		return approval.Outcome{}, projectRefusal(ctx, undoProjectGone)
	}
	s.logger.InfoContext(ctx, "project.changed", "principal_id", record.PrincipalID, "project_id", undo.ProjectID, "tool", record.Tool, "undo", true)
	s.changed(ctx, after)
	if record.Tool == toolCreate {
		return approval.Outcome{Text: abandonedOutcome.FormatIn(locale.English, after.Title), UserText: abandonedOutcome.Format(ctx, after.Title)}, nil
	}
	return approval.Outcome{Text: restoredOutcome.FormatIn(locale.English, after.Title), UserText: restoredOutcome.Format(ctx, after.Title)}, nil
}

func errorResult(text string) tools.Result {
	return tools.Result{Text: text, IsError: true}
}
