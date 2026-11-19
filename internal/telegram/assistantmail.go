package telegram

import (
	"context"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/locale"
)

const assistantMailSectionPrefix = "mailbox"

var (
	AssistantMailOnLine  = locale.T("Assistant mailbox: on (the default). I read the mail that arrives at your address on the assistant's mailbox and tell you here when a message needs you, and I can read it for you and write to businesses from it. Nothing is sent until you confirm the exact message.", "Почта ассистента: включена (по умолчанию). Я читаю письма, которые приходят на твой адрес в почте ассистента, и сообщаю тебе здесь, когда письмо требует твоего внимания, могу прочитать его и писать с него компаниям. Ничего не отправляется, пока ты не подтвердишь точный текст.")
	AssistantMailOffLine = locale.T("Assistant mailbox: off. Turn it back on to be told when mail at your address on the assistant's mailbox needs you, and to let me read it and write from it.", "Почта ассистента: выключена. Включи её снова, чтобы получать сообщения о письмах на твой адрес в почте ассистента, которые требуют твоего внимания, и чтобы я мог читать их и писать с этого адреса.")
	// AssistantMailPrivacyLine is added to /privacy while the principal's mailbox switch is on.
	AssistantMailPrivacyLine = locale.T("Assistant mailbox: each new message at your address goes to your model, which decides whether it needs you; the sender and subject of mail I tell you about go to your model with your next message, and nothing is sent without your confirmation.", "Почта ассистента: каждое новое письмо на твой адрес уходит твоей модели, которая решает, требует ли оно твоего внимания; отправитель и тема писем, о которых я сообщаю, уходят твоей модели вместе с твоим следующим сообщением, и ничего не отправляется без твоего подтверждения.")
	assistantMailAddress     = locale.T("Your address on the assistant's mailbox, which you can give to businesses; mail sent to it reaches only you:\n%s", "Твой адрес в почте ассистента, который можно давать компаниям; письма на него видишь только ты:\n%s")
)

// AssistantMailPage is the on/off page of the assistant's own mailbox.
var AssistantMailPage = SwitchPage{
	Integration: "assistant-mail", Prefix: assistantMailSectionPrefix, Name: locale.T("Assistant mailbox", "Почта ассистента"),
	OnLine: AssistantMailOnLine, OffLine: AssistantMailOffLine,
	OnButton: locale.T("Turn on the mailbox", "Включить почту ассистента"), OffButton: locale.T("Turn off the mailbox", "Выключить почту ассистента"),
	Event: "assistantmail.changed", HelpTopic: "assistant_mailbox", PrivacyLine: AssistantMailPrivacyLine,
}

// AssistantMailSettings is the mailbox page: the switch page plus the principal's own address.
type AssistantMailSettings struct {
	Switch SwitchSettings
	// Address is the principal's address on the mailbox; the page is shown only to principals that have one.
	Address func(principalID string) (string, bool)
}

// NewAssistantMailSection is the /settings page that shows a principal's address on the assistant's
// mailbox and turns the mailbox on and off.
func NewAssistantMailSection(settings AssistantMailSettings, logger *slog.Logger, telemetry Telemetry) Section {
	hasAddress := func(principalID string) bool {
		_, found := settings.Address(principalID)
		return found
	}
	settings.Switch.Allowed = hasAddress
	return assistantMailSection{switchSection: switchSection{settings: settings.Switch, logger: logger, telemetry: telemetryOrNoop(telemetry)}, address: settings.Address}
}

type assistantMailSection struct {
	switchSection
	address func(principalID string) (string, bool)
}

// render puts the address under the page's description, in a code span so it can be copied.
func (a assistantMailSection) render(ctx context.Context, principalID string) (settingsView, error) {
	view, err := a.switchSection.render(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if address, found := a.address(principalID); found {
		lines := append([]string{view.lines[0], assistantMailAddress.Format(ctx, "`"+address+"`")}, view.lines[1:]...)
		view.lines = lines
	}
	return view, nil
}
