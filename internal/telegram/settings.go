package telegram

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/connectors/nas"
	"github.com/vladar107/home-ai-assistant/internal/connectors/notion"
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

const (
	maximumCallbackDataBytes = 64
	// labelCallbackData marks a button that only labels the rows below it; pressing it changes
	// nothing, so the settings message is not edited.
	labelCallbackData = "label"
)

var (
	SettingsTitle         = locale.T("Settings", "Настройки")
	SettingsPrivacyLine   = locale.T("Privacy mode: strict-local. Use /privacy for details and /status to check that I am online.", "Режим приватности: strict-local. Команда /privacy расскажет подробнее, а /status покажет, что я на связи.")
	ModelNotConnectedLine = locale.T("Model subscription: not connected.", "Подписка на модель: не подключена.")
	ModelCommandLine      = locale.T("Choose the model and effort of a conversation with /model.", "Выбрать модель и глубину рассуждений разговора можно командой /model.")
)

var errUnknownSettingsAction = errors.New("unknown settings action")

// Section is one /settings page: it renders a principal's settings and performs the actions behind
// the buttons it rendered. Callback data is "<prefix>:<action>", and settingsGroups places the page
// by its prefix. A section may also add lines to /privacy (privacySection), name an app_help topic
// (helpSection), or consume messages (messageInterceptor).
type Section interface {
	prefix() string
	// summary is the section's state in a few words, for the overview and its group page.
	summary(context.Context, string) (settingsSummary, error)
	render(context.Context, string) (settingsView, error)
	// act performs a validated action; an unknown action yields errUnknownSettingsAction.
	act(ctx context.Context, principalID string, chatID int64, action string) (settingsOutcome, error)
}

type pressTriggerKey struct{}

// withPressTrigger hands a section's act the press that caused it, for an action that queues work
// once per press.
func withPressTrigger(ctx context.Context, trigger string) context.Context {
	return context.WithValue(ctx, pressTriggerKey{}, trigger)
}

func pressTriggerFrom(ctx context.Context) string {
	trigger, _ := ctx.Value(pressTriggerKey{}).(string)
	return trigger
}

// privacySection adds its lines to /privacy, in the place privacyOrder gives its prefix.
type privacySection interface {
	privacy(ctx context.Context, principalID string) ([]string, error)
}

// helpSection names the app_help topic that explains the section's page.
type helpSection interface {
	helpTopic() string
}

// verbSection names the action a press is recorded as when the action carries an item's ID, so the
// recorded command has one value per action.
type verbSection interface {
	verb(action string) string
}

// restrictedSection is a section only some principals may see, such as one the operator configured
// for themselves alone.
type restrictedSection interface {
	availableTo(principalID string) bool
}

// sectionsFor is the sections the principal may see; a restricted section is absent for everyone
// else, so its buttons resolve to nothing even when forged.
func sectionsFor(sections []Section, principalID string) []Section {
	visible := make([]Section, 0, len(sections))
	for _, section := range sections {
		if restricted, ok := section.(restrictedSection); ok && !restricted.availableTo(principalID) {
			continue
		}
		visible = append(visible, section)
	}
	return visible
}

type settingsSummary struct {
	name  string
	state string
	// attention names something the user has to act on; the overview flags it.
	attention string
}

type settingsOutcome struct {
	// note confirms the action on the refreshed page.
	note string
	// message is sent on its own because the user acts on it, such as sign-in instructions.
	message string
}

type settingsView struct {
	lines   []string
	buttons []InlineButton
	// rows are further keyboard rows, for a section with a row per item.
	rows [][]InlineButton
}

// messageInterceptor lets a section consume an ordinary message before it is enqueued for the
// model, for deterministic intake such as a pasted token. guard only keeps a secret out of the
// model, consuming no intake, for a caption or transcript that cannot be an intake reply.
type messageInterceptor interface {
	intercept(ctx context.Context, principalID string, text string) (interception, error)
	guard(ctx context.Context, principalID, text string) interception
}

// intakeSection is an interceptor whose intake waits for a principal's next message, whatever it
// looks like. That message stands alone rather than joining a burst, a caption is offered to it as
// a text message is, and it yields a message another section's guard recognises unless claims
// says the text is its own.
type intakeSection interface {
	awaiting(principalID string) bool
	claims(principalID, text string) bool
	yield(principalID string)
}

type interception struct {
	handled bool
	// remove deletes the user's message before the reply is sent.
	remove bool
	reply  string
	reason string
}

const (
	// settingsNavPrefix callbacks name the page to show: "nav:root", "nav:<group>", or
	// "nav:<group>.<section>". Pages are derived from the data alone, so nothing is stored.
	settingsNavPrefix = "nav"
	settingsRootPage  = "root"
	// settingsBackText is the English label of the Back button; settingsBackLabel is the one drawn.
	settingsBackText = "‹ Back"
	attentionMark    = "⚠"
)

type settingsGroup struct {
	id    string
	title locale.Text
	// inline groups show their sections' controls on the group page; the others link to a page
	// per section.
	inline  bool
	members []string
}

// sectionOrder is the order of the sections whatever order they were passed in: app_help lists
// their topics in it, and the message interceptors run in it. The services intake comes before
// every token guard, so a key the user was asked for is taken even when it looks like another
// service's token.
var sectionOrder = []string{
	languageSectionPrefix,
	modelSectionPrefix,
	mcpSectionPrefix,
	notionSectionPrefix,
	nasSectionPrefix,
	googleSectionPrefix,
	webSectionPrefix,
	nsSectionPrefix,
	flightsSectionPrefix,
	homeAssistantSectionPrefix,
	busSectionPrefix,
	headsUpSectionPrefix,
	typeSafeSectionPrefix,
	widgetSectionPrefix,
	locationSectionPrefix,
	grafanaSectionPrefix,
	eventsSectionPrefix,
	memorySectionPrefix,
	tripsSectionPrefix,
	inboxSectionPrefix,
}

// settingsGroups orders the overview; a section missing from it is never shown.
var settingsGroups = []settingsGroup{
	{id: "language", title: locale.T("Language", "Язык"), inline: true, members: []string{languageSectionPrefix}},
	{id: "model", title: locale.T("Model", "Модель"), members: []string{modelSectionPrefix}},
	{id: "accounts", title: locale.T("Accounts", "Аккаунты"), members: []string{googleSectionPrefix, notionSectionPrefix}},
	{id: "home", title: locale.T("Home & files", "Дом и файлы"), inline: true, members: []string{homeAssistantSectionPrefix, nasSectionPrefix}},
	{id: "tools", title: locale.T("Everyday tools", "Повседневные инструменты"), inline: true, members: []string{webSectionPrefix, nsSectionPrefix, flightsSectionPrefix, busSectionPrefix, headsUpSectionPrefix, tripsSectionPrefix, inboxSectionPrefix}},
	{id: "services", title: locale.T("Services", "Сервисы"), inline: true, members: []string{mcpSectionPrefix}},
	{id: "location", title: locale.T("Location", "Местоположение"), inline: true, members: []string{locationSectionPrefix}},
	{id: "memory", title: locale.T("Memory", "Память"), inline: true, members: []string{memorySectionPrefix}},
	{id: "monitoring", title: locale.T("Monitoring", "Мониторинг"), inline: true, members: []string{grafanaSectionPrefix}},
	{id: "events", title: locale.T("Events", "События"), inline: true, members: []string{eventsSectionPrefix}},
	{id: "widgets", title: locale.T("Widgets", "Виджеты"), inline: true, members: []string{widgetSectionPrefix}},
	{id: "safety", title: locale.T("Safety & privacy", "Безопасность и приватность"), inline: true, members: []string{typeSafeSectionPrefix}},
}

func (g settingsGroup) titleIn(ctx context.Context) string {
	return g.title.Format(ctx)
}

func settingsMembers(sections []Section, group settingsGroup) []Section {
	var members []Section
	for _, prefix := range group.members {
		for _, section := range sections {
			if section.prefix() == prefix {
				members = append(members, section)
			}
		}
	}
	return members
}

// showsInline reports whether a group page carries its sections' controls; a group with a single
// configured section needs no page in between.
func showsInline(group settingsGroup, members []Section) bool {
	return group.inline || len(members) == 1
}

// settingsPageOf is the page that holds a section's controls, shown again after its actions.
func settingsPageOf(sections []Section, prefix string) string {
	for _, group := range settingsGroups {
		if !slices.Contains(group.members, prefix) {
			continue
		}
		if showsInline(group, settingsMembers(sections, group)) {
			return group.id
		}
		return group.id + "." + prefix
	}
	return settingsRootPage
}

func navButton(text, page string) InlineButton {
	return InlineButton{Text: text, CallbackData: settingsNavPrefix + ":" + page}
}

func flagged(text string, attention bool) string {
	if attention {
		return text + " " + attentionMark
	}
	return text
}

// stateOf words a switch for the user's language.
func stateOf(ctx context.Context, enabled bool) string {
	if enabled {
		return locale.On.Format(ctx)
	}
	return locale.Off.Format(ctx)
}

func settingsBackLabel(ctx context.Context) string {
	return "‹ " + locale.Back.Format(ctx)
}

func crumbs(ctx context.Context, titles ...string) string {
	return strings.Join(append([]string{SettingsTitle.Format(ctx)}, titles...), " › ")
}

// renderSettings renders a page; an unknown page falls back to the overview.
func renderSettings(ctx context.Context, principalID string, sections []Section, page, note string) (string, InlineKeyboard, error) {
	groupID, sectionPrefix, _ := strings.Cut(page, ".")
	for _, group := range settingsGroups {
		if group.id != groupID {
			continue
		}
		members := settingsMembers(sections, group)
		if len(members) == 0 {
			break
		}
		if sectionPrefix == "" || showsInline(group, members) {
			return renderSettingsGroup(ctx, principalID, group, members, note)
		}
		for _, section := range members {
			if section.prefix() == sectionPrefix {
				return renderSettingsSection(ctx, principalID, group, section, note)
			}
		}
	}
	return renderSettingsOverview(ctx, principalID, sections)
}

func renderSettingsOverview(ctx context.Context, principalID string, sections []Section) (string, InlineKeyboard, error) {
	var attention, overview []string
	var buttons []InlineButton
	for _, group := range settingsGroups {
		members := settingsMembers(sections, group)
		if len(members) == 0 {
			continue
		}
		states := make([]string, 0, len(members))
		needsAttention := false
		for _, section := range members {
			summary, err := section.summary(ctx, principalID)
			if err != nil {
				return "", nil, err
			}
			if len(group.members) == 1 {
				states = append(states, summary.state)
			} else {
				states = append(states, summary.name+" "+summary.state)
			}
			if summary.attention != "" {
				attention = append(attention, attentionMark+" "+summary.attention)
				needsAttention = true
			}
		}
		overview = append(overview, group.titleIn(ctx)+": "+strings.Join(states, ", "))
		buttons = append(buttons, navButton(flagged(group.titleIn(ctx), needsAttention), group.id))
	}
	lines := []string{SettingsTitle.Format(ctx), ""}
	if len(attention) > 0 {
		lines = append(append(lines, attention...), "")
	}
	lines = append(append(lines, overview...), "", SettingsPrivacyLine.Format(ctx))
	var keyboard InlineKeyboard
	for start := 0; start < len(buttons); start += 2 {
		keyboard = append(keyboard, buttons[start:min(start+2, len(buttons))])
	}
	return strings.Join(lines, "\n"), keyboard, nil
}

func renderSettingsGroup(ctx context.Context, principalID string, group settingsGroup, members []Section, note string) (string, InlineKeyboard, error) {
	lines := []string{crumbs(ctx, group.titleIn(ctx)), ""}
	var keyboard InlineKeyboard
	if showsInline(group, members) {
		for i, section := range members {
			view, err := section.render(ctx, principalID)
			if err != nil {
				return "", nil, err
			}
			if i > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, view.lines...)
			keyboard = appendView(keyboard, view)
		}
	} else {
		var row []InlineButton
		for _, section := range members {
			summary, err := section.summary(ctx, principalID)
			if err != nil {
				return "", nil, err
			}
			lines = append(lines, summary.name+": "+summary.state)
			if summary.attention != "" {
				lines = append(lines, attentionMark+" "+summary.attention)
			}
			row = append(row, navButton(flagged(summary.name, summary.attention != ""), group.id+"."+section.prefix()))
		}
		keyboard = append(keyboard, row)
	}
	return finishPage(ctx, lines, keyboard, note, settingsRootPage)
}

func renderSettingsSection(ctx context.Context, principalID string, group settingsGroup, section Section, note string) (string, InlineKeyboard, error) {
	summary, err := section.summary(ctx, principalID)
	if err != nil {
		return "", nil, err
	}
	view, err := section.render(ctx, principalID)
	if err != nil {
		return "", nil, err
	}
	lines := append([]string{crumbs(ctx, group.titleIn(ctx), summary.name), ""}, view.lines...)
	return finishPage(ctx, lines, appendView(nil, view), note, group.id)
}

func appendView(keyboard InlineKeyboard, view settingsView) InlineKeyboard {
	if len(view.buttons) > 0 {
		keyboard = append(keyboard, view.buttons)
	}
	return append(keyboard, view.rows...)
}

func finishPage(ctx context.Context, lines []string, keyboard InlineKeyboard, note, back string) (string, InlineKeyboard, error) {
	if note != "" {
		lines = append(lines, "", note)
	}
	keyboard = append(keyboard, []InlineButton{navButton(settingsBackLabel(ctx), back)})
	return strings.Join(lines, "\n"), keyboard, nil
}

// settingsNavFor resolves a navigation callback to its page.
func settingsNavFor(data string) (string, bool) {
	prefix, page, found := strings.Cut(data, ":")
	if prefix != settingsNavPrefix || !found || page == "" || len(data) > maximumCallbackDataBytes {
		return "", false
	}
	return page, true
}

// settingsSectionFor resolves callback data to its section and action; anything outside the fixed
// vocabulary resolves to nothing so a stale or forged button only refreshes the view.
func settingsSectionFor(sections []Section, data string) (Section, string, bool) {
	if len(data) > maximumCallbackDataBytes {
		return nil, "", false
	}
	prefix, action, found := strings.Cut(data, ":")
	if !found || action == "" {
		return nil, "", false
	}
	for _, section := range sections {
		if section.prefix() == prefix {
			return section, action, true
		}
	}
	return nil, "", false
}

const (
	modelSectionPrefix    = "model"
	modelConnectAction    = "connect"
	modelDisconnectAction = "disconnect"
)

var (
	modelName              = locale.T("Model", "Модель")
	modelNotConnectedState = locale.T("not connected", "не подключена")
	modelConnectedState    = locale.T("connected", "подключена")
	modelConnectAttention  = locale.T("Connect your model subscription to start talking to me.", "Подключите подписку на модель, чтобы начать со мной разговор.")
	modelConnectButton     = locale.T("Connect model", "Подключить модель")
	modelDisconnectButton  = locale.T("Disconnect model", "Отключить модель")
)

// NewModelSection is the page that connects and disconnects the principal's model subscription;
// disconnecting stops the principal's running turns first when conversation is set.
func NewModelSection(connections ConnectionController, conversation ConversationController, logger *slog.Logger, telemetry Telemetry) Section {
	return modelSection{connections: connections, conversation: conversation, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type modelSection struct {
	connections  ConnectionController
	conversation ConversationController
	logger       *slog.Logger
	telemetry    Telemetry
}

func (m modelSection) prefix() string { return modelSectionPrefix }

func (m modelSection) helpTopic() string { return "model" }

func (m modelSection) privacy(ctx context.Context, _ string) ([]string, error) {
	return []string{modelUsagePrivacyLine.Format(ctx)}, nil
}

func (m modelSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	status, err := m.connections.Status(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	if !status.Connected {
		return settingsSummary{name: modelName.Format(ctx), state: modelNotConnectedState.Format(ctx), attention: modelConnectAttention.Format(ctx)}, nil
	}
	return settingsSummary{name: modelName.Format(ctx), state: modelConnectedState.Format(ctx)}, nil
}

func (m modelSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := m.connections.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if !status.Connected {
		return settingsView{
			lines:   []string{ModelNotConnectedLine.Format(ctx)},
			buttons: []InlineButton{{Text: modelConnectButton.Format(ctx), CallbackData: modelSectionPrefix + ":" + modelConnectAction}},
		}, nil
	}
	return settingsView{
		lines:   []string{ModelConnectedLine(ctx, status), ModelCommandLine.Format(ctx)},
		buttons: []InlineButton{{Text: modelDisconnectButton.Format(ctx), CallbackData: modelSectionPrefix + ":" + modelDisconnectAction}},
	}, nil
}

func (m modelSection) act(ctx context.Context, principalID string, chatID int64, action string) (settingsOutcome, error) {
	switch action {
	case modelConnectAction:
		ceremony, err := m.connections.Connect(ctx, principalID, chatID)
		if err != nil {
			m.telemetry.ContextLogger(ctx, m.logger).WarnContext(ctx, "telegram.connection.start_failed", "principal_id", principalID, "error", err)
			return settingsOutcome{message: connection.ConnectFailedText.Format(ctx)}, nil
		}
		return settingsOutcome{message: ConnectInstructions(ctx, ceremony)}, nil
	case modelDisconnectAction:
		if m.conversation != nil {
			if _, err := m.conversation.CancelAll(ctx, principalID); err != nil {
				return settingsOutcome{}, err
			}
		}
		if _, err := m.connections.Disconnect(ctx, principalID); err != nil {
			return settingsOutcome{}, err
		}
		return settingsOutcome{}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

const (
	notionSectionPrefix    = "notion"
	notionConnectAction    = "connect"
	notionDisconnectAction = "disconnect"
	notionWritesOnAction   = "writes-on"
	notionWritesOffAction  = "writes-off"
)

var (
	NotionUnavailableLine     = locale.T("Notion: the installed token could not be verified because Notion is unavailable. Try again later.", "Notion: установленный токен не удалось проверить, потому что Notion недоступен. Повторите попытку позже.")
	NotionConnectFailedReply  = locale.T("Notion could not be reached to verify the token, so nothing was stored. Please try again later.", "Не удалось связаться с Notion, чтобы проверить токен, поэтому ничего не сохранено. Повторите попытку позже.")
	NotionWritesOnReply       = locale.T("Notion writes are on. The model may propose to create or update pages; nothing is written until you confirm the exact proposal.", "Запись в Notion включена. Модель может предлагать создать или обновить страницы; ничего не записывается, пока вы не подтвердите конкретное предложение.")
	NotionWritesOffReply      = locale.T("Notion writes are off. The model can only read your Notion content.", "Запись в Notion выключена. Модель может только читать ваше содержимое Notion.")
	NotionNotConnectedLine    = locale.T("Notion: not connected.", "Notion: не подключён.")
	NotionOperatorLine        = locale.T("Notion: not connected. Ask the operator to install your token with scripts/notion-set-token.", "Notion: не подключён. Попросите оператора установить ваш токен с помощью scripts/notion-set-token.")
	NotionInvalidTokenLine    = locale.T("Notion: the installed token could not be used. Install a fresh token or disconnect.", "Notion: установленный токен не удалось использовать. Установите новый токен или отключите Notion.")
	NotionPrivacyLine         = locale.T("Notion pages I read go to your model.", "Страницы Notion, которые я читаю, уходят вашей модели.")
	NotionConnectInstructions = locale.T("Send your Notion token as your next message within 10 minutes. I will delete that message right away and never show the token again. "+
		"Recommended: an internal connection with read, update, and insert content capabilities, shared only with the pages and data sources you want me to see. "+
		"A personal access token also works, but it grants everything you can access and expires. "+
		"Notion content read through the connection is sent to your own model subscription.", "Отправьте токен Notion следующим сообщением в течение 10 минут. Я сразу удалю это сообщение и никогда больше не покажу токен. Рекомендуется внутреннее подключение с правами на чтение, обновление и вставку содержимого, к которому открыт доступ только для страниц и источников данных, которые вы хотите мне показать. Подойдёт и персональный токен доступа, но он даёт всё, к чему у вас есть доступ, и истекает. Содержимое Notion, прочитанное через подключение, отправляется вашей собственной подписке на модель.")
	NotionNotATokenReply    = locale.T("That did not look like a Notion token, so nothing was stored and it was not sent to the model. Press Connect Notion again when you have one.", "Это не похоже на токен Notion, поэтому ничего не сохранено и модели не отправлено. Когда он у вас будет, снова нажмите «Подключить Notion».")
	NotionInvalidTokenReply = locale.T("Notion rejected that token, so nothing was stored. Check it in Notion's developer portal and press Connect Notion again.", "Notion отклонил этот токен, поэтому ничего не сохранено. Проверьте его на портале разработчиков Notion и снова нажмите «Подключить Notion».")
	NotionTokenGuardReply   = locale.T("That looked like a Notion token, so I deleted it and did not send it to the model. To connect Notion, use Connect Notion in /settings or ask the operator to install it.", "Это было похоже на токен Notion, поэтому сообщение удалено и модели не отправлено. Чтобы подключить Notion, нажмите «Подключить Notion» в /settings или попросите оператора установить его.")
	DeleteFailedLine        = locale.T("I could not delete your message; please delete it yourself.", "Не удалось удалить ваше сообщение; удалите его, пожалуйста, сами.")
)

var (
	notionPersonalKind = locale.T("personal access token", "персональный токен доступа")
	notionInternalKind = locale.T("internal connection", "внутреннее подключение")

	notionConnectedLine          = locale.T("Notion: connected as %[1]q (%[2]s), writes %[3]s.", "Notion: подключён как %[1]q (%[2]s), запись %[3]s.")
	notionConnectedWorkspaceLine = locale.T("Notion: connected to workspace %[1]q as %[2]q (%[3]s), writes %[4]s.", "Notion: подключён к рабочему пространству %[1]q как %[2]q (%[3]s), запись %[4]s.")
	notionConnectedReply         = locale.T("Notion is connected as %[1]q (%[2]s).", "Notion подключён как %[1]q (%[2]s).")
	notionConnectedWorkspace     = locale.T("Notion is connected to workspace %[1]q as %[2]q (%[3]s).", "Notion подключён к рабочему пространству %[1]q как %[2]q (%[3]s).")
	notionWritesOn               = locale.T("on", "включена")
	notionWritesOff              = locale.T("off", "выключена")
)

func notionKind(ctx context.Context, status notion.Status) string {
	if status.TokenKind == "personal" {
		return notionPersonalKind.Format(ctx)
	}
	return notionInternalKind.Format(ctx)
}

func notionConnectedLineIn(ctx context.Context, status notion.Status) string {
	writes := notionWritesOff.Format(ctx)
	if status.WritesEnabled {
		writes = notionWritesOn.Format(ctx)
	}
	if status.WorkspaceName == "" {
		return notionConnectedLine.Format(ctx, status.BotName, notionKind(ctx, status), writes)
	}
	return notionConnectedWorkspaceLine.Format(ctx, status.WorkspaceName, status.BotName, notionKind(ctx, status), writes)
}

func notionConnectedReplyIn(ctx context.Context, status notion.Status) string {
	if status.WorkspaceName == "" {
		return notionConnectedReply.Format(ctx, status.BotName, notionKind(ctx, status))
	}
	return notionConnectedWorkspace.Format(ctx, status.WorkspaceName, status.BotName, notionKind(ctx, status))
}

// NotionConnectedLine is the connected line in English, as a test reads it.
func NotionConnectedLine(status notion.Status) string {
	return notionConnectedLineIn(context.Background(), status)
}

// NotionConnectedReply is the connected reply in English, as a test reads it.
func NotionConnectedReply(status notion.Status) string {
	return notionConnectedReplyIn(context.Background(), status)
}

// NewNotionSection is the page that connects Notion; it also takes the token pasted after Connect
// Notion and keeps any token-looking message from the model.
func NewNotionSection(notion NotionController, logger *slog.Logger, telemetry Telemetry) Section {
	return notionSection{notion: notion, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type notionSection struct {
	notion    NotionController
	logger    *slog.Logger
	telemetry Telemetry
}

func (n notionSection) prefix() string { return notionSectionPrefix }

func (n notionSection) helpTopic() string { return "notion" }

func (n notionSection) privacy(ctx context.Context, _ string) ([]string, error) {
	return []string{NotionPrivacyLine.Format(ctx)}, nil
}

var (
	notionRejectedState     = locale.T("token rejected", "токен отклонён")
	notionRejectedAttention = locale.T("Notion rejected your token; install a fresh one or disconnect.", "Notion отклонил ваш токен; установите новый или отключите Notion.")
	notionUnavailableState  = locale.T("unavailable", "недоступен")
	notionWritesOnState     = locale.T("writes on", "запись включена")
	notionWritesOffState    = locale.T("writes off", "запись выключена")
	notionNotConnectedState = locale.T("not connected", "не подключён")
	notionDisconnectButton  = locale.T("Disconnect Notion", "Отключить Notion")
	notionWritesOnButton    = locale.T("Turn Notion writes on", "Включить запись в Notion")
	notionWritesOffButton   = locale.T("Turn Notion writes off", "Выключить запись в Notion")
	notionConnectButton     = locale.T("Connect Notion", "Подключить Notion")
)

func (n notionSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	status, err := n.notion.Status(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	switch {
	case status.Problem == notion.ProblemInvalidToken:
		return settingsSummary{name: "Notion", state: notionRejectedState.Format(ctx), attention: notionRejectedAttention.Format(ctx)}, nil
	case status.Problem == notion.ProblemUnavailable:
		return settingsSummary{name: "Notion", state: notionUnavailableState.Format(ctx)}, nil
	case status.Connected:
		if status.WritesEnabled {
			return settingsSummary{name: "Notion", state: notionWritesOnState.Format(ctx)}, nil
		}
		return settingsSummary{name: "Notion", state: notionWritesOffState.Format(ctx)}, nil
	default:
		return settingsSummary{name: "Notion", state: notionNotConnectedState.Format(ctx)}, nil
	}
}

func (n notionSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := n.notion.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	disconnect := []InlineButton{{Text: notionDisconnectButton.Format(ctx), CallbackData: notionSectionPrefix + ":" + notionDisconnectAction}}
	switch {
	case status.Problem == notion.ProblemInvalidToken:
		return settingsView{lines: []string{NotionInvalidTokenLine.Format(ctx)}, buttons: disconnect}, nil
	case status.Problem == notion.ProblemUnavailable:
		return settingsView{lines: []string{NotionUnavailableLine.Format(ctx)}, buttons: disconnect}, nil
	case status.Connected:
		writes := InlineButton{Text: notionWritesOnButton.Format(ctx), CallbackData: notionSectionPrefix + ":" + notionWritesOnAction}
		if status.WritesEnabled {
			writes = InlineButton{Text: notionWritesOffButton.Format(ctx), CallbackData: notionSectionPrefix + ":" + notionWritesOffAction}
		}
		return settingsView{lines: []string{notionConnectedLineIn(ctx, status)}, buttons: append([]InlineButton{writes}, disconnect...)}, nil
	case n.notion.ChatConnectEnabled():
		return settingsView{
			lines:   []string{NotionNotConnectedLine.Format(ctx)},
			buttons: []InlineButton{{Text: notionConnectButton.Format(ctx), CallbackData: notionSectionPrefix + ":" + notionConnectAction}},
		}, nil
	default:
		return settingsView{lines: []string{NotionOperatorLine.Format(ctx)}}, nil
	}
}

func (n notionSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	switch action {
	case notionConnectAction:
		if !n.notion.ChatConnectEnabled() {
			return settingsOutcome{}, errUnknownSettingsAction
		}
		n.notion.BeginIntake(principalID)
		n.telemetry.ContextLogger(ctx, n.logger).InfoContext(ctx, "notion.intake.started", "principal_id", principalID)
		return settingsOutcome{message: NotionConnectInstructions.Format(ctx)}, nil
	case notionDisconnectAction:
		if _, err := n.notion.Disconnect(ctx, principalID); err != nil {
			return settingsOutcome{}, err
		}
		return settingsOutcome{}, nil
	case notionWritesOnAction, notionWritesOffAction:
		enabled := action == notionWritesOnAction
		status, err := n.notion.SetWrites(ctx, principalID, enabled)
		if err != nil {
			return settingsOutcome{}, err
		}
		if !status.Connected {
			return settingsOutcome{}, nil
		}
		n.telemetry.ContextLogger(ctx, n.logger).InfoContext(ctx, "notion.writes.changed", "principal_id", principalID, "enabled", enabled)
		if enabled {
			return settingsOutcome{note: NotionWritesOnReply.Format(ctx)}, nil
		}
		return settingsOutcome{note: NotionWritesOffReply.Format(ctx)}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

// intercept consumes the message after Connect Notion as the token, and keeps any token-looking
// message out of the model whether or not an intake is pending. The text is never logged.
func (n notionSection) intercept(ctx context.Context, principalID string, text string) (interception, error) {
	if n.notion.ConsumeIntake(principalID) {
		if !notion.LooksLikeToken(text) {
			return interception{handled: true, reply: NotionNotATokenReply.Format(ctx), reason: "notion_intake_not_token"}, nil
		}
		status, err := n.notion.Connect(ctx, principalID, text)
		switch {
		case errors.Is(err, notion.ErrInvalidToken):
			return interception{handled: true, remove: true, reply: NotionInvalidTokenReply.Format(ctx), reason: "notion_intake_rejected"}, nil
		case err != nil:
			n.telemetry.ContextLogger(ctx, n.logger).WarnContext(ctx, "notion.intake.failed", "principal_id", principalID, "error", err)
			return interception{handled: true, remove: true, reply: NotionConnectFailedReply.Format(ctx), reason: "notion_intake_failed"}, nil
		default:
			return interception{handled: true, remove: true, reply: notionConnectedReplyIn(ctx, status), reason: "notion_intake_connected"}, nil
		}
	}
	return n.guard(ctx, principalID, text), nil
}

func (n notionSection) guard(ctx context.Context, _ string, text string) interception {
	if !notion.LooksLikeToken(text) {
		return interception{}
	}
	return interception{handled: true, remove: true, reply: NotionTokenGuardReply.Format(ctx), reason: "notion_token_guard"}
}

const (
	nasSectionPrefix   = "nas"
	nasWritesOnAction  = "writes-on"
	nasWritesOffAction = "writes-off"
)

var (
	NASWritesOnReply         = locale.T("Saving to the NAS is on. The model may propose to file a photo or document you sent in the conversation; nothing is written until you confirm the exact proposal, and a save can only add a new file.", "Сохранение на NAS включено. Модель может предложить сохранить фото или документ, отправленные вами в разговоре; ничего не записывается, пока вы не подтвердите конкретное предложение, а сохранение может только добавить новый файл.")
	NASWritesOffReply        = locale.T("Saving to the NAS is off. The model can browse and read your NAS folders, but not add anything.", "Сохранение на NAS выключено. Модель может просматривать и читать ваши папки на NAS, но не может ничего добавлять.")
	NASNoAccessLine          = locale.T("NAS: no folders are configured for you.", "NAS: для вас не настроено ни одной папки.")
	nasReadOnlyLine          = locale.T("NAS: %s (read-only).", "NAS: %s (только чтение).")
	nasSavingLine            = locale.T("NAS: %[1]s. Saving %[2]s, to %[3]s.", "NAS: %[1]s. Сохранение: %[2]s, в %[3]s.")
	nasNoFoldersState        = locale.T("no folders", "нет папок")
	nasReadOnlyState         = locale.T("read-only", "только чтение")
	nasSavingOnState         = locale.T("saving on", "сохранение включено")
	nasSavingOffState        = locale.T("saving off", "сохранение выключено")
	nasSavingOnButton        = locale.T("Turn NAS saving on", "Включить сохранение на NAS")
	nasSavingOffButton       = locale.T("Turn NAS saving off", "Выключить сохранение на NAS")
	NASSavingUnavailableLine = locale.T("Saving is not configured for you, so NAS files can only be read.", "Сохранение для вас не настроено, поэтому файлы на NAS можно только читать.")
)

// NASConnectedLine names the folders this user can reach and whether saving is on, because those
// two facts are the whole of what the connector can do for them.
func NASConnectedLine(ctx context.Context, status nas.Status) string {
	names := make([]string, 0, len(status.Roots))
	for _, root := range status.Roots {
		names = append(names, root.Name)
	}
	if !status.CanSave {
		return nasReadOnlyLine.Format(ctx, strings.Join(names, ", "))
	}
	return nasSavingLine.Format(ctx, strings.Join(names, ", "), stateOf(ctx, status.WritesEnabled), status.DefaultFolder)
}

// nasSection renders the NAS folders. There is nothing to connect or disconnect: access is the
// set of directories mounted for this user, which only the operator can change.
// NewNASSection is the page that shows the principal's NAS folders and turns saving on and off.
func NewNASSection(nas NASController, logger *slog.Logger, telemetry Telemetry) Section {
	return nasSection{nas: nas, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type nasSection struct {
	nas       NASController
	logger    *slog.Logger
	telemetry Telemetry
}

func (n nasSection) prefix() string { return nasSectionPrefix }

func (n nasSection) helpTopic() string { return "nas" }

func (n nasSection) privacy(ctx context.Context, _ string) ([]string, error) {
	return []string{SentFilePrivacyLine.Format(ctx)}, nil
}

func (n nasSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	status, err := n.nas.Status(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	switch {
	case !status.Connected:
		return settingsSummary{name: "NAS", state: nasNoFoldersState.Format(ctx)}, nil
	case !status.CanSave:
		return settingsSummary{name: "NAS", state: nasReadOnlyState.Format(ctx)}, nil
	case status.WritesEnabled:
		return settingsSummary{name: "NAS", state: nasSavingOnState.Format(ctx)}, nil
	default:
		return settingsSummary{name: "NAS", state: nasSavingOffState.Format(ctx)}, nil
	}
}

func (n nasSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := n.nas.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	switch {
	case !status.Connected:
		return settingsView{lines: []string{NASNoAccessLine.Format(ctx)}}, nil
	case !status.CanSave:
		return settingsView{lines: []string{NASConnectedLine(ctx, status), NASSavingUnavailableLine.Format(ctx)}}, nil
	default:
		writes := InlineButton{Text: nasSavingOnButton.Format(ctx), CallbackData: nasSectionPrefix + ":" + nasWritesOnAction}
		if status.WritesEnabled {
			writes = InlineButton{Text: nasSavingOffButton.Format(ctx), CallbackData: nasSectionPrefix + ":" + nasWritesOffAction}
		}
		return settingsView{lines: []string{NASConnectedLine(ctx, status)}, buttons: []InlineButton{writes}}, nil
	}
}

func (n nasSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	switch action {
	case nasWritesOnAction, nasWritesOffAction:
		enabled := action == nasWritesOnAction
		status, err := n.nas.SetWrites(ctx, principalID, enabled)
		if err != nil {
			return settingsOutcome{}, err
		}
		if !status.Connected || !status.CanSave {
			return settingsOutcome{}, nil
		}
		n.telemetry.ContextLogger(ctx, n.logger).InfoContext(ctx, "nas.writes.changed", "principal_id", principalID, "enabled", enabled)
		if enabled {
			return settingsOutcome{note: NASWritesOnReply.Format(ctx)}, nil
		}
		return settingsOutcome{note: NASWritesOffReply.Format(ctx)}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}
