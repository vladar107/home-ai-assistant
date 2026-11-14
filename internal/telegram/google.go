package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

const (
	googleSectionPrefix    = "google"
	googleConnectAction    = "connect"
	googleDisconnectAction = "disconnect"
)

var (
	googleSignInSteps = locale.T("To connect a Google account read-only:\n"+
		"1. Open the link below and sign in with the Google account to connect. Google will say the app is unverified; it is the household's own app, so continue.\n"+
		"2. Allow reading your email, your calendar, or both. Nothing can send, change, or delete anything.\n"+
		"3. Your browser then fails to open a localhost page. Copy that page's full address and send it here within 10 minutes. I will delete it right away.\n\n",
		"Чтобы подключить аккаунт Google только для чтения:\n"+
			"1. Откройте ссылку ниже и войдите в аккаунт Google, который хотите подключить. Google сообщит, что приложение не проверено; это собственное приложение вашей семьи, так что продолжайте.\n"+
			"2. Разрешите чтение почты, календаря или того и другого. Ничего нельзя ни отправить, ни изменить, ни удалить.\n"+
			"3. Затем браузер не сможет открыть страницу localhost. Скопируйте полный адрес этой страницы и отправьте его сюда в течение 10 минут. Я сразу его удалю.\n\n")
	googleLinkFollows = locale.T("(a personal sign-in link follows)", "(следом придёт персональная ссылка для входа)")

	GoogleAccountsLine       = locale.T("Google accounts:", "Аккаунты Google:")
	GooglePrivacyLine        = locale.T("Mail and calendar events I read go to your model.", "Почта и события календаря, которые я читаю, уходят вашей модели.")
	GoogleRejectedReply      = locale.T("Google refused that sign-in, so nothing was connected. Codes work once and only for a few minutes; press Connect a Google account in /settings to try again.", "Google отклонил этот вход, поэтому ничего не подключено. Коды работают один раз и только несколько минут; нажмите «Подключить аккаунт Google» в /settings, чтобы попробовать снова.")
	GoogleConnectFailedReply = locale.T("Google could not be reached to finish the sign-in, so nothing was connected. Please try again later.", "Не удалось связаться с Google, чтобы завершить вход, поэтому ничего не подключено. Повторите попытку позже.")

	googleNotAllowedState  = locale.T("not allowed", "не разрешено")
	googleReconnectLine    = locale.T("%s: Google no longer accepts the connection. Reconnect or disconnect.", "%s: Google больше не принимает подключение. Подключите аккаунт заново или отключите его.")
	googleAccountLine      = locale.T("%[1]s: Mail %[2]s, Calendar %[3]s.", "%[1]s: почта: %[2]s, календарь: %[3]s.")
	googleConnectedReply   = locale.T("%[1]s is connected read-only: Mail %[2]s, Calendar %[3]s.", "Аккаунт %[1]s подключён только для чтения: почта: %[2]s, календарь: %[3]s.")
	googleNotConnected     = locale.T("not connected", "не подключён")
	googleAccountCount     = locale.N("%[1]d account", "%[1]d accounts", "%[1]d аккаунт", "%[1]d аккаунта", "%[1]d аккаунтов")
	googleReconnectNote    = locale.T("Reconnect %s in Accounts › Google.", "Подключите заново %s в разделе «Аккаунты › Google».")
	googleConnectButton    = locale.T("Connect a Google account", "Подключить аккаунт Google")
	googleDisconnectLabel  = locale.T("Disconnect", "Отключить")
	googleMailOn           = locale.T("Mail on", "Включить почту")
	googleMailOff          = locale.T("Mail off", "Выключить почту")
	googleCalendarOn       = locale.T("Calendar on", "Включить календарь")
	googleCalendarOff      = locale.T("Calendar off", "Выключить календарь")
	GoogleNotConnectedLine = locale.T("Google: no accounts connected.", "Google: аккаунты не подключены.")
	GoogleNoSignInReply    = locale.T("That sign-in address does not match a pending sign-in, or it expired, so nothing was connected. Press Connect a Google account in /settings to start again. I deleted the message.", "Этот адрес входа не соответствует ожидающему входу или устарел, поэтому ничего не подключено. Нажмите «Подключить аккаунт Google» в /settings, чтобы начать заново. Сообщение удалено.")
	GoogleDeniedReply      = locale.T("Neither Gmail nor Calendar access was granted, so nothing was connected. Press Connect a Google account in /settings and allow at least one.", "Доступ не дан ни к Gmail, ни к Календарю, поэтому ничего не подключено. Нажмите «Подключить аккаунт Google» в /settings и разрешите хотя бы один из них.")
	GoogleTakenReply       = locale.T("That Google account is already connected for another household member, so it was not connected for you.", "Этот аккаунт Google уже подключён у другого члена семьи, поэтому для вас он не подключён.")
	GoogleNotGrantedReply  = locale.T("That account did not allow it when it was connected. Press Connect a Google account, sign in with the same account, and leave it ticked.", "При подключении этот аккаунт не дал такого разрешения. Нажмите «Подключить аккаунт Google», войдите в тот же аккаунт и оставьте галочку отмеченной.")
	GoogleSignInGuardReply = locale.T("That looked like a Google sign-in address, so I deleted it and did not send it to the model. To finish signing in, send the address on its own as a text message.", "Это было похоже на адрес входа в Google, поэтому сообщение удалено и модели не отправлено. Чтобы завершить вход, отправьте адрес отдельным текстовым сообщением.")
)

// GoogleController is implemented by the Google connector, for the Google and Trips settings
// sections.
type GoogleController interface {
	Status(context.Context, string) ([]google.AccountStatus, error)
	BeginSignIn(string) (string, error)
	CompleteSignIn(context.Context, string, string) (google.AccountStatus, error)
	SetProduct(ctx context.Context, principalID, accountID string, product google.Product, enabled bool) (bool, error)
	Disconnect(ctx context.Context, principalID, accountID string) (bool, error)
}

func productState(ctx context.Context, granted, enabled bool) string {
	if !granted {
		return googleNotAllowedState.Format(ctx)
	}
	return stateOf(ctx, enabled)
}

func GoogleAccountLine(ctx context.Context, status google.AccountStatus) string {
	if status.NeedsReconnect {
		return googleReconnectLine.Format(ctx, status.Address)
	}
	return googleAccountLine.Format(ctx, status.Address,
		productState(ctx, status.MailGranted, status.MailEnabled), productState(ctx, status.CalendarGranted, status.CalendarEnabled))
}

func GoogleConnectedReply(ctx context.Context, status google.AccountStatus) string {
	return googleConnectedReply.Format(ctx, status.Address,
		productState(ctx, status.MailGranted, status.MailEnabled), productState(ctx, status.CalendarGranted, status.CalendarEnabled))
}

// NewGoogleSection is the page that connects Google accounts; it also takes the sign-in address
// pasted back and keeps any sign-in address from the model.
func NewGoogleSection(google GoogleController, logger *slog.Logger, telemetry Telemetry) Section {
	return googleSection{google: google, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type googleSection struct {
	google    GoogleController
	logger    *slog.Logger
	telemetry Telemetry
}

func (g googleSection) prefix() string { return googleSectionPrefix }

func (g googleSection) helpTopic() string { return "google" }

func (g googleSection) privacy(ctx context.Context, _ string) ([]string, error) {
	return []string{GooglePrivacyLine.Format(ctx)}, nil
}

func (g googleSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	statuses, err := g.google.Status(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	summary := settingsSummary{name: "Google", state: googleNotConnected.Format(ctx)}
	if len(statuses) > 0 {
		summary.state = googleAccountCount.Format(ctx, len(statuses))
	}
	var stale []string
	for _, status := range statuses {
		if status.NeedsReconnect {
			stale = append(stale, status.Address)
		}
	}
	if len(stale) > 0 {
		summary.attention = googleReconnectNote.Format(ctx, strings.Join(stale, ", "))
	}
	return summary, nil
}

// render lists every account under a header button showing its address, followed by a row of its
// action buttons.
func (g googleSection) render(ctx context.Context, principalID string) (settingsView, error) {
	statuses, err := g.google.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	view := settingsView{buttons: []InlineButton{{Text: googleConnectButton.Format(ctx), CallbackData: googleSectionPrefix + ":" + googleConnectAction}}}
	if len(statuses) == 0 {
		view.lines = []string{GoogleNotConnectedLine.Format(ctx)}
		return view, nil
	}
	view.lines = []string{GoogleAccountsLine.Format(ctx)}
	for _, status := range statuses {
		view.lines = append(view.lines, GoogleAccountLine(ctx, status))
		view.rows = append(view.rows, []InlineButton{{Text: status.Address, CallbackData: labelCallbackData}})
		var row []InlineButton
		if !status.NeedsReconnect {
			if status.MailGranted {
				row = append(row, productButton(ctx, status.ID, google.ProductMail, status.MailEnabled))
			}
			if status.CalendarGranted {
				row = append(row, productButton(ctx, status.ID, google.ProductCalendar, status.CalendarEnabled))
			}
		}
		row = append(row, InlineButton{
			Text:         googleDisconnectLabel.Format(ctx),
			CallbackData: googleSectionPrefix + ":" + googleDisconnectAction + ":" + status.ID,
		})
		view.rows = append(view.rows, row)
	}
	return view, nil
}

func productButton(ctx context.Context, accountID string, product google.Product, enabled bool) InlineButton {
	label, state := googleMailOn, "on"
	switch {
	case product == google.ProductMail && enabled:
		label, state = googleMailOff, "off"
	case product == google.ProductCalendar && enabled:
		label, state = googleCalendarOff, "off"
	case product == google.ProductCalendar:
		label = googleCalendarOn
	}
	return InlineButton{
		Text:         label.Format(ctx),
		CallbackData: googleSectionPrefix + ":" + string(product) + "-" + state + ":" + accountID,
	}
}

func (g googleSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	if action == googleConnectAction {
		link, err := g.google.BeginSignIn(principalID)
		if err != nil {
			return settingsOutcome{}, err
		}
		g.telemetry.ContextLogger(ctx, g.logger).InfoContext(ctx, "google.signin.started", "principal_id", principalID)
		return settingsOutcome{message: googleSignInSteps.Format(ctx) + link}, nil
	}
	verb, accountID, found := strings.Cut(action, ":")
	if !found || accountID == "" {
		return settingsOutcome{}, errUnknownSettingsAction
	}
	switch verb {
	case googleDisconnectAction:
		_, err := g.google.Disconnect(ctx, principalID, accountID)
		return settingsOutcome{}, err
	case "mail-on", "mail-off", "calendar-on", "calendar-off":
		product, state, _ := strings.Cut(verb, "-")
		_, err := g.google.SetProduct(ctx, principalID, accountID, google.Product(product), state == "on")
		if errors.Is(err, google.ErrNotGranted) {
			return settingsOutcome{note: GoogleNotGrantedReply.Format(ctx)}, nil
		}
		return settingsOutcome{}, err
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

// intercept consumes any message carrying a sign-in address, pending sign-in or not, so an
// authorization code never reaches the model or stays in the chat. The text is never logged.
func (g googleSection) intercept(ctx context.Context, principalID string, text string) (interception, error) {
	if !google.LooksLikeRedirect(text) {
		return interception{}, nil
	}
	status, err := g.google.CompleteSignIn(ctx, principalID, text)
	result := interception{handled: true, remove: true}
	switch {
	case err == nil:
		result.reply, result.reason = GoogleConnectedReply(ctx, status), "google_signin_connected"
	case errors.Is(err, google.ErrNoSignIn):
		result.reply, result.reason = GoogleNoSignInReply.Format(ctx), "google_signin_unmatched"
	case errors.Is(err, google.ErrDenied):
		result.reply, result.reason = GoogleDeniedReply.Format(ctx), "google_signin_denied"
	case errors.Is(err, google.ErrRejected):
		result.reply, result.reason = GoogleRejectedReply.Format(ctx), "google_signin_rejected"
	case errors.Is(err, google.ErrAccountTaken):
		result.reply, result.reason = GoogleTakenReply.Format(ctx), "google_signin_taken"
	default:
		g.telemetry.ContextLogger(ctx, g.logger).WarnContext(ctx, "google.signin.failed", "principal_id", principalID, "error", err)
		result.reply, result.reason = GoogleConnectFailedReply.Format(ctx), "google_signin_failed"
	}
	return result, nil
}

func (g googleSection) guard(ctx context.Context, _ string, text string) interception {
	if !google.LooksLikeRedirect(text) {
		return interception{}
	}
	return interception{handled: true, remove: true, reply: GoogleSignInGuardReply.Format(ctx), reason: "google_signin_guard"}
}
