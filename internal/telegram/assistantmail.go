package telegram

import (
	"context"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/locale"
)

const (
	assistantMailSectionPrefix = "mailbox"
	repliesOnAction            = "replies_on"
	repliesOffAction           = "replies_off"
)

var (
	AssistantMailOnLine  = locale.T("Assistant mailbox: on (the default). I read the mail that arrives at your address on the assistant's mailbox and tell you here when a message needs you, and I can read it for you and write to businesses from it. Nothing is sent until you confirm the exact message, except routine replies in threads I started for you, when you turn them on.", "Почта ассистента: включена (по умолчанию). Я читаю письма, которые приходят на твой адрес в почте ассистента, и сообщаю тебе здесь, когда письмо требует твоего внимания, могу прочитать его и писать с него компаниям. Ничего не отправляется, пока ты не подтвердишь точный текст, кроме рутинных ответов в переписке, которую я начал для тебя, если ты их включишь.")
	AssistantMailOffLine = locale.T("Assistant mailbox: off. Turn it back on to be told when mail at your address on the assistant's mailbox needs you, and to let me read it and write from it.", "Почта ассистента: выключена. Включи её снова, чтобы получать сообщения о письмах на твой адрес в почте ассистента, которые требуют твоего внимания, и чтобы я мог читать их и писать с этого адреса.")
	// AssistantMailPrivacyLine is added to /privacy while the principal's mailbox switch is on.
	AssistantMailPrivacyLine = locale.T("Assistant mailbox: each new message at your address goes to your model, which decides whether it needs you; the sender and subject of mail I tell you about go to your model with your next message, and nothing is sent without your confirmation, except routine replies in threads I started for you, when you turn them on.", "Почта ассистента: каждое новое письмо на твой адрес уходит твоей модели, которая решает, требует ли оно твоего внимания; отправитель и тема писем, о которых я сообщаю, уходят твоей модели вместе с твоим следующим сообщением, и ничего не отправляется без твоего подтверждения, кроме рутинных ответов в переписке, которую я начал для тебя, если ты их включишь.")
	// AssistantMailRepliesOnLine and AssistantMailRepliesOffLine describe the routine-reply switch,
	// shown on the page while the mailbox is on.
	AssistantMailRepliesOnLine  = locale.T("Routine replies: on. In a thread I started for you, I answer a routine message from someone I wrote to, such as thanks, an acknowledgement, or something the thread already settled, without asking you: at most once per thread and three times a day, only to a verified sender, never automated mail. I show you every reply in full; anything else still waits for your confirmation.", "Рутинные ответы: включены. В переписке, которую я начал для тебя, я отвечаю без вопросов на рутинное письмо от того, кому писал, например на благодарность, подтверждение получения или то, о чём уже договорились: не больше одного раза в переписке и трёх раз в день, только подтверждённому отправителю и никогда на автоматические письма. Каждый такой ответ я показываю тебе целиком; всё остальное по-прежнему ждёт твоего подтверждения.")
	AssistantMailRepliesOffLine = locale.T("Routine replies: off (the default). Turn them on to let me answer routine messages, such as thanks or an acknowledgement, in threads I started for you without asking: at most once per thread and three times a day, and you see every reply in full.", "Рутинные ответы: выключены (по умолчанию). Включи их, чтобы я без вопросов отвечал на рутинные письма, например на благодарность или подтверждение получения, в переписке, которую я начал для тебя: не больше одного раза в переписке и трёх раз в день, и каждый ответ ты видишь целиком.")
	// AssistantMailRepliesPrivacyLine is added to /privacy while the principal has routine replies on.
	AssistantMailRepliesPrivacyLine = locale.T("Routine replies: in a thread I started for you, a short routine reply to someone I wrote to goes out without your confirmation, and you see each one in full.", "Рутинные ответы: в переписке, которую я начал для тебя, короткий рутинный ответ тому, кому я писал, уходит без твоего подтверждения, и каждый такой ответ ты видишь целиком.")
	assistantMailRepliesOn          = locale.T("Turn on routine replies", "Включить рутинные ответы")
	assistantMailRepliesOff         = locale.T("Turn off routine replies", "Выключить рутинные ответы")
	assistantMailAddress            = locale.T("Your address on the assistant's mailbox, which you can give to businesses; mail sent to it reaches only you:\n%s", "Твой адрес в почте ассистента, который можно давать компаниям; письма на него видишь только ты:\n%s")
)

// AssistantMailPage is the on/off page of the assistant's own mailbox.
var AssistantMailPage = SwitchPage{
	Integration: "assistant-mail", Prefix: assistantMailSectionPrefix, Name: locale.T("Assistant mailbox", "Почта ассистента"),
	OnLine: AssistantMailOnLine, OffLine: AssistantMailOffLine,
	OnButton: locale.T("Turn on the mailbox", "Включить почту ассистента"), OffButton: locale.T("Turn off the mailbox", "Выключить почту ассистента"),
	Event: "assistantmail.changed", HelpTopic: "assistant_mailbox", PrivacyLine: AssistantMailPrivacyLine,
}

// AssistantMailSettings is the mailbox page: the switch page plus the principal's own address, and
// their routine-reply switch.
type AssistantMailSettings struct {
	Switch SwitchSettings
	// Address is the principal's address on the mailbox; the page is shown only to principals that have one.
	Address func(principalID string) (string, bool)
	// Replies is the principal's routine-reply switch, shown while the mailbox is on; nil shows none.
	Replies SwitchController
}

// NewAssistantMailSection is the /settings page that shows a principal's address on the assistant's
// mailbox and turns the mailbox on and off.
func NewAssistantMailSection(settings AssistantMailSettings, logger *slog.Logger, telemetry Telemetry) Section {
	hasAddress := func(principalID string) bool {
		_, found := settings.Address(principalID)
		return found
	}
	settings.Switch.Allowed = hasAddress
	return assistantMailSection{
		switchSection: switchSection{settings: settings.Switch, logger: logger, telemetry: telemetryOrNoop(telemetry)},
		address:       settings.Address, replies: settings.Replies,
	}
}

type assistantMailSection struct {
	switchSection
	address func(principalID string) (string, bool)
	replies SwitchController
}

// render puts the address under the page's description, in a code span so it can be copied, and,
// while the mailbox is on, the routine-reply switch on its own row.
func (a assistantMailSection) render(ctx context.Context, principalID string) (settingsView, error) {
	view, err := a.switchSection.render(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if address, found := a.address(principalID); found {
		lines := append([]string{view.lines[0], assistantMailAddress.Format(ctx, "`"+address+"`")}, view.lines[1:]...)
		view.lines = lines
	}
	if a.replies == nil {
		return view, nil
	}
	if enabled, err := a.settings.Toggle.Enabled(ctx, principalID); err != nil || !enabled {
		return view, err
	}
	replies, err := a.replies.Enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	line, button, action := AssistantMailRepliesOffLine, assistantMailRepliesOn, repliesOnAction
	if replies {
		line, button, action = AssistantMailRepliesOnLine, assistantMailRepliesOff, repliesOffAction
	}
	view.lines = append(view.lines, line.Format(ctx))
	view.rows = append(view.rows, []InlineButton{{Text: button.Format(ctx), CallbackData: a.prefix() + ":" + action}})
	return view, nil
}

// privacy adds the routine-reply line while the mailbox and its routine replies are both on.
func (a assistantMailSection) privacy(ctx context.Context, principalID string) ([]string, error) {
	lines, err := a.switchSection.privacy(ctx, principalID)
	if err != nil || len(lines) == 0 || a.replies == nil {
		return lines, err
	}
	replies, err := a.replies.Enabled(ctx, principalID)
	if err != nil || !replies {
		return lines, err
	}
	return append(lines, AssistantMailRepliesPrivacyLine.Format(ctx)), nil
}

func (a assistantMailSection) act(ctx context.Context, principalID string, chatID int64, action string) (settingsOutcome, error) {
	if action != repliesOnAction && action != repliesOffAction || a.replies == nil {
		return a.switchSection.act(ctx, principalID, chatID, action)
	}
	enabled := action == repliesOnAction
	if err := a.replies.SetEnabled(ctx, principalID, enabled); err != nil {
		return settingsOutcome{}, err
	}
	a.telemetry.ContextLogger(ctx, a.logger).InfoContext(ctx, "assistantmail.replies.changed", "principal_id", principalID, "enabled", enabled)
	return settingsOutcome{}, nil
}
