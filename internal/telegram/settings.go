package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/connectors/nas"
	"github.com/vladar107/home-ai-assistant/internal/connectors/notion"
)

const (
	SettingsTitle         = "Settings"
	SettingsPrivacyLine   = "Privacy mode: strict-local. Use /privacy for details and /status to check that I am online."
	ModelNotConnectedLine = "Model subscription: not connected."

	maximumCallbackDataBytes = 64
	// labelCallbackData marks a button that only labels the rows below it; pressing it changes
	// nothing, so the settings message is not edited.
	labelCallbackData = "label"
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
	guard(text string) interception
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
	settingsBackText  = "‹ Back"
	attentionMark     = "⚠"
)

type settingsGroup struct {
	id    string
	title string
	// inline groups show their sections' controls on the group page; the others link to a page
	// per section.
	inline  bool
	members []string
}

// sectionOrder is the order of the sections whatever order they were passed in: app_help lists
// their topics in it, and the message interceptors run in it.
var sectionOrder = []string{
	modelSectionPrefix,
	notionSectionPrefix,
	nasSectionPrefix,
	googleSectionPrefix,
	webSectionPrefix,
	nsSectionPrefix,
	homeAssistantSectionPrefix,
	busSectionPrefix,
	headsUpSectionPrefix,
	typeSafeSectionPrefix,
	locationSectionPrefix,
	grafanaSectionPrefix,
	memorySectionPrefix,
	tripsSectionPrefix,
}

// settingsGroups orders the overview; a section missing from it is never shown.
var settingsGroups = []settingsGroup{
	{id: "model", title: "Model", members: []string{modelSectionPrefix}},
	{id: "accounts", title: "Accounts", members: []string{googleSectionPrefix, notionSectionPrefix}},
	{id: "home", title: "Home & files", inline: true, members: []string{homeAssistantSectionPrefix, nasSectionPrefix}},
	{id: "tools", title: "Everyday tools", inline: true, members: []string{webSectionPrefix, nsSectionPrefix, busSectionPrefix, headsUpSectionPrefix, tripsSectionPrefix}},
	{id: "location", title: "Location", inline: true, members: []string{locationSectionPrefix}},
	{id: "memory", title: "Memory", inline: true, members: []string{memorySectionPrefix}},
	{id: "monitoring", title: "Monitoring", inline: true, members: []string{grafanaSectionPrefix}},
	{id: "safety", title: "Safety & privacy", inline: true, members: []string{typeSafeSectionPrefix}},
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

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func crumbs(titles ...string) string {
	return strings.Join(append([]string{SettingsTitle}, titles...), " › ")
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
		overview = append(overview, group.title+": "+strings.Join(states, ", "))
		buttons = append(buttons, navButton(flagged(group.title, needsAttention), group.id))
	}
	lines := []string{SettingsTitle, ""}
	if len(attention) > 0 {
		lines = append(append(lines, attention...), "")
	}
	lines = append(append(lines, overview...), "", SettingsPrivacyLine)
	var keyboard InlineKeyboard
	for start := 0; start < len(buttons); start += 2 {
		keyboard = append(keyboard, buttons[start:min(start+2, len(buttons))])
	}
	return strings.Join(lines, "\n"), keyboard, nil
}

func renderSettingsGroup(ctx context.Context, principalID string, group settingsGroup, members []Section, note string) (string, InlineKeyboard, error) {
	lines := []string{crumbs(group.title), ""}
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
	return finishPage(lines, keyboard, note, settingsRootPage)
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
	lines := append([]string{crumbs(group.title, summary.name), ""}, view.lines...)
	return finishPage(lines, appendView(nil, view), note, group.id)
}

func appendView(keyboard InlineKeyboard, view settingsView) InlineKeyboard {
	if len(view.buttons) > 0 {
		keyboard = append(keyboard, view.buttons)
	}
	return append(keyboard, view.rows...)
}

func finishPage(lines []string, keyboard InlineKeyboard, note, back string) (string, InlineKeyboard, error) {
	if note != "" {
		lines = append(lines, "", note)
	}
	keyboard = append(keyboard, []InlineButton{navButton(settingsBackText, back)})
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

func (m modelSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	status, err := m.connections.Status(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	if !status.Connected {
		return settingsSummary{name: "Model", state: "not connected", attention: "Connect your model subscription to start talking to me."}, nil
	}
	return settingsSummary{name: "Model", state: "connected"}, nil
}

func (m modelSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := m.connections.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if !status.Connected {
		return settingsView{
			lines:   []string{ModelNotConnectedLine},
			buttons: []InlineButton{{Text: "Connect model", CallbackData: modelSectionPrefix + ":" + modelConnectAction}},
		}, nil
	}
	return settingsView{
		lines:   []string{ModelConnectedLine(status)},
		buttons: []InlineButton{{Text: "Disconnect model", CallbackData: modelSectionPrefix + ":" + modelDisconnectAction}},
	}, nil
}

func (m modelSection) act(ctx context.Context, principalID string, chatID int64, action string) (settingsOutcome, error) {
	switch action {
	case modelConnectAction:
		ceremony, err := m.connections.Connect(ctx, principalID, chatID)
		if err != nil {
			m.telemetry.ContextLogger(ctx, m.logger).WarnContext(ctx, "telegram.connection.start_failed", "principal_id", principalID, "error", err)
			return settingsOutcome{message: connection.ConnectFailedReply}, nil
		}
		return settingsOutcome{message: ConnectInstructions(ceremony)}, nil
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

	NotionWritesOnReply  = "Notion writes are on. The model may propose to create or update pages; nothing is written until you confirm the exact proposal."
	NotionWritesOffReply = "Notion writes are off. The model can only read your Notion content."

	NotionNotConnectedLine    = "Notion: not connected."
	NotionOperatorLine        = "Notion: not connected. Ask the operator to install your token with scripts/notion-set-token."
	NotionInvalidTokenLine    = "Notion: the installed token could not be used. Install a fresh token or disconnect."
	NotionUnavailableLine     = "Notion: the installed token could not be verified because Notion is unavailable. Try again later."
	NotionPrivacyLine         = "Notion pages I read go to your model."
	NotionConnectInstructions = "Send your Notion token as your next message within 10 minutes. I will delete that message right away and never show the token again. " +
		"Recommended: an internal connection with read, update, and insert content capabilities, shared only with the pages and data sources you want me to see. " +
		"A personal access token also works, but it grants everything you can access and expires. " +
		"Notion content read through the connection is sent to your own model subscription."
	NotionNotATokenReply     = "That did not look like a Notion token, so nothing was stored and it was not sent to the model. Press Connect Notion again when you have one."
	NotionInvalidTokenReply  = "Notion rejected that token, so nothing was stored. Check it in Notion's developer portal and press Connect Notion again."
	NotionConnectFailedReply = "Notion could not be reached to verify the token, so nothing was stored. Please try again later."
	NotionTokenGuardReply    = "That looked like a Notion token, so I deleted it and did not send it to the model. To connect Notion, use Connect Notion in /settings or ask the operator to install it."
	DeleteFailedLine         = "I could not delete your message; please delete it yourself."
)

func notionKind(status notion.Status) string {
	if status.TokenKind == "personal" {
		return "personal access token"
	}
	return "internal connection"
}

func NotionConnectedLine(status notion.Status) string {
	writes := "off"
	if status.WritesEnabled {
		writes = "on"
	}
	if status.WorkspaceName == "" {
		return fmt.Sprintf("Notion: connected as %q (%s), writes %s.", status.BotName, notionKind(status), writes)
	}
	return fmt.Sprintf("Notion: connected to workspace %q as %q (%s), writes %s.", status.WorkspaceName, status.BotName, notionKind(status), writes)
}

func NotionConnectedReply(status notion.Status) string {
	if status.WorkspaceName == "" {
		return fmt.Sprintf("Notion is connected as %q (%s).", status.BotName, notionKind(status))
	}
	return fmt.Sprintf("Notion is connected to workspace %q as %q (%s).", status.WorkspaceName, status.BotName, notionKind(status))
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

func (n notionSection) privacy(context.Context, string) ([]string, error) {
	return []string{NotionPrivacyLine}, nil
}

func (n notionSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	status, err := n.notion.Status(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	switch {
	case status.Problem == notion.ProblemInvalidToken:
		return settingsSummary{name: "Notion", state: "token rejected", attention: "Notion rejected your token; install a fresh one or disconnect."}, nil
	case status.Problem == notion.ProblemUnavailable:
		return settingsSummary{name: "Notion", state: "unavailable"}, nil
	case status.Connected:
		return settingsSummary{name: "Notion", state: "writes " + onOff(status.WritesEnabled)}, nil
	default:
		return settingsSummary{name: "Notion", state: "not connected"}, nil
	}
}

func (n notionSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := n.notion.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	disconnect := []InlineButton{{Text: "Disconnect Notion", CallbackData: notionSectionPrefix + ":" + notionDisconnectAction}}
	switch {
	case status.Problem == notion.ProblemInvalidToken:
		return settingsView{lines: []string{NotionInvalidTokenLine}, buttons: disconnect}, nil
	case status.Problem == notion.ProblemUnavailable:
		return settingsView{lines: []string{NotionUnavailableLine}, buttons: disconnect}, nil
	case status.Connected:
		writes := InlineButton{Text: "Turn Notion writes on", CallbackData: notionSectionPrefix + ":" + notionWritesOnAction}
		if status.WritesEnabled {
			writes = InlineButton{Text: "Turn Notion writes off", CallbackData: notionSectionPrefix + ":" + notionWritesOffAction}
		}
		return settingsView{lines: []string{NotionConnectedLine(status)}, buttons: append([]InlineButton{writes}, disconnect...)}, nil
	case n.notion.ChatConnectEnabled():
		return settingsView{
			lines:   []string{NotionNotConnectedLine},
			buttons: []InlineButton{{Text: "Connect Notion", CallbackData: notionSectionPrefix + ":" + notionConnectAction}},
		}, nil
	default:
		return settingsView{lines: []string{NotionOperatorLine}}, nil
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
		return settingsOutcome{message: NotionConnectInstructions}, nil
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
			return settingsOutcome{note: NotionWritesOnReply}, nil
		}
		return settingsOutcome{note: NotionWritesOffReply}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

// intercept consumes the message after Connect Notion as the token, and keeps any token-looking
// message out of the model whether or not an intake is pending. The text is never logged.
func (n notionSection) intercept(ctx context.Context, principalID string, text string) (interception, error) {
	if n.notion.ConsumeIntake(principalID) {
		if !notion.LooksLikeToken(text) {
			return interception{handled: true, reply: NotionNotATokenReply, reason: "notion_intake_not_token"}, nil
		}
		status, err := n.notion.Connect(ctx, principalID, text)
		switch {
		case errors.Is(err, notion.ErrInvalidToken):
			return interception{handled: true, remove: true, reply: NotionInvalidTokenReply, reason: "notion_intake_rejected"}, nil
		case err != nil:
			n.telemetry.ContextLogger(ctx, n.logger).WarnContext(ctx, "notion.intake.failed", "principal_id", principalID, "error", err)
			return interception{handled: true, remove: true, reply: NotionConnectFailedReply, reason: "notion_intake_failed"}, nil
		default:
			return interception{handled: true, remove: true, reply: NotionConnectedReply(status), reason: "notion_intake_connected"}, nil
		}
	}
	return n.guard(text), nil
}

func (n notionSection) guard(text string) interception {
	if !notion.LooksLikeToken(text) {
		return interception{}
	}
	return interception{handled: true, remove: true, reply: NotionTokenGuardReply, reason: "notion_token_guard"}
}

const (
	nasSectionPrefix   = "nas"
	nasWritesOnAction  = "writes-on"
	nasWritesOffAction = "writes-off"

	NASWritesOnReply  = "Saving to the NAS is on. The model may propose to file a photo or document you sent in the conversation; nothing is written until you confirm the exact proposal, and a save can only add a new file."
	NASWritesOffReply = "Saving to the NAS is off. The model can browse and read your NAS folders, but not add anything."

	NASNoAccessLine          = "NAS: no folders are configured for you."
	NASSavingUnavailableLine = "Saving is not configured for you, so NAS files can only be read."
)

// NASConnectedLine names the folders this user can reach and whether saving is on, because those
// two facts are the whole of what the connector can do for them.
func NASConnectedLine(status nas.Status) string {
	names := make([]string, 0, len(status.Roots))
	for _, root := range status.Roots {
		names = append(names, root.Name)
	}
	if !status.CanSave {
		return fmt.Sprintf("NAS: %s (read-only).", strings.Join(names, ", "))
	}
	writes := "off"
	if status.WritesEnabled {
		writes = "on"
	}
	return fmt.Sprintf("NAS: %s. Saving %s, to %s.", strings.Join(names, ", "), writes, status.DefaultFolder)
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

func (n nasSection) privacy(context.Context, string) ([]string, error) {
	return []string{SentFilePrivacyLine}, nil
}

func (n nasSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	status, err := n.nas.Status(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	switch {
	case !status.Connected:
		return settingsSummary{name: "NAS", state: "no folders"}, nil
	case !status.CanSave:
		return settingsSummary{name: "NAS", state: "read-only"}, nil
	default:
		return settingsSummary{name: "NAS", state: "saving " + onOff(status.WritesEnabled)}, nil
	}
}

func (n nasSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := n.nas.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	switch {
	case !status.Connected:
		return settingsView{lines: []string{NASNoAccessLine}}, nil
	case !status.CanSave:
		return settingsView{lines: []string{NASConnectedLine(status), NASSavingUnavailableLine}}, nil
	default:
		writes := InlineButton{Text: "Turn NAS saving on", CallbackData: nasSectionPrefix + ":" + nasWritesOnAction}
		if status.WritesEnabled {
			writes = InlineButton{Text: "Turn NAS saving off", CallbackData: nasSectionPrefix + ":" + nasWritesOffAction}
		}
		return settingsView{lines: []string{NASConnectedLine(status)}, buttons: []InlineButton{writes}}, nil
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
			return settingsOutcome{note: NASWritesOnReply}, nil
		}
		return settingsOutcome{note: NASWritesOffReply}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}
