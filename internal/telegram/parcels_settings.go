package telegram

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
)

const (
	parcelsSectionPrefix = "parcels"
	parcelsOnAction      = "on"
	parcelsOffAction     = "off"
	parcelsPostcode      = "postcode"
	parcelsClearPostcode = "clear"

	// parcelsIntakeLifetime is how long the page waits for the postcode after its button is pressed.
	parcelsIntakeLifetime = 10 * time.Minute

	maximumParcelSourceTitleRunes = 40

	// ParcelsHelp is added to the app_help topic of the page.
	ParcelsHelp = "Each parcel the user is waiting for has a card in the Parcels tab (the main view with tabs off), a widget while Widgets is on and a message otherwise. The card shows the carrier, the title, the sender, the status, the expected day and window, and the pickup point, and links to the carrier's page. Stop tracking is a button on the card, or a control inside the widget. The user gets a separate message when a parcel is out for delivery, ready for pickup, delivered, or has a problem; the rest of the changes only edit the card. The page also holds the postcode some carrier integrations ask for, set by pressing its button and sending the postcode as the next message."
)

var (
	parcelsName               = locale.T("Parcels", "Посылки")
	parcelsSourcesHeader      = locale.T("Home Assistant accounts assigned to you:", "Аккаунты Home Assistant, назначенные тебе:")
	parcelsNoSources          = locale.T("No Home Assistant account is assigned to you; I follow your parcels through your shipping mail.", "Тебе не назначен аккаунт Home Assistant; я слежу за твоими посылками по письмам о доставке.")
	parcelsSourceLine         = locale.T("• %[1]s (%[2]s): %[3]s", "• %[1]s (%[2]s): %[3]s")
	parcelsSourceOK           = locale.T("reading", "читается")
	parcelsSourceStale        = locale.T("not reading, showing the last parcels", "не читается, показаны последние посылки")
	parcelsSourceMissing      = locale.T("not found in Home Assistant", "не найден в Home Assistant")
	parcelsSourceUnknown      = locale.T("not read yet", "ещё не прочитан")
	parcelsPostcodeLine       = locale.T("Postcode: %s", "Почтовый индекс: %s")
	parcelsNoPostcode         = locale.T("Postcode: not set. Some carrier integrations need it to add a tracking number.", "Почтовый индекс: не указан. Некоторым интеграциям служб доставки он нужен, чтобы добавить номер отслеживания.")
	parcelsMailReadable       = locale.T("Mail: readable.", "Почта: доступна.")
	parcelsMailBlocked        = locale.T("Mail: not readable. Connect Google in Accounts to follow parcels through shipping mail.", "Почта: недоступна. Подключи Google в «Аккаунтах», чтобы следить за посылками по письмам о доставке.")
	parcelsMailUnknown        = locale.T("Mail: could not be checked.", "Почта: не удалось проверить.")
	parcelsSourcesStale       = locale.T("Parcels: a Home Assistant account is not reading.", "Посылки: аккаунт Home Assistant не читается.")
	parcelsSetButton          = locale.T("Set postcode", "Указать индекс")
	parcelsChangeButton       = locale.T("Change postcode", "Изменить индекс")
	parcelsClearButton        = locale.T("Clear postcode", "Удалить индекс")
	parcelsPostcodeAsk        = locale.T("Send your postcode as your next message within %d minutes. It is kept for you only and goes to the Home Assistant carrier integration, which uses it with the carrier, only when a tracking number is added.", "Пришли почтовый индекс следующим сообщением в течение %d мин. Он хранится только для тебя и уходит интеграции службы доставки в Home Assistant, которая использует его вместе с названием службы, только когда добавляется номер отслеживания.")
	parcelsSourcesUnknown     = locale.T("Home Assistant accounts: could not be checked.", "Аккаунты Home Assistant: не удалось проверить.")
	parcelsSourcesUnknownNote = locale.T("Parcels: Home Assistant accounts could not be checked.", "Посылки: не удалось проверить аккаунты Home Assistant.")
	parcelsPostcodeSet        = locale.T("Postcode: set.", "Почтовый индекс: указан.")
	parcelsPostcodeFailed     = locale.T("I could not save the postcode. Send it again.", "Не удалось сохранить индекс. Пришли его ещё раз.")
	parcelsCatchUpNote        = locale.T("Parcels will catch up with this in a moment.", "Посылки подстроятся под это через минуту.")
	parcelsPostcodeSaved      = locale.T("Postcode saved.", "Индекс сохранён.")
	parcelsPostcodeBad        = locale.T("That does not look like a postcode, so I kept the old one. Press Set postcode to try again.", "Это не похоже на почтовый индекс, поэтому прежний сохранён. Нажми «Указать индекс», чтобы попробовать снова.")
	parcelsPostcodeGone       = locale.T("Postcode removed.", "Индекс удалён.")
)

type helpViewKey struct{}

// withHelpView marks a render that app_help hands to the model, which never shows the postcode.
func withHelpView(ctx context.Context) context.Context {
	return context.WithValue(ctx, helpViewKey{}, true)
}

var postcodePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 -]{1,10}[A-Za-z0-9]$`)

// ParcelMail says whether the principal's mail can be read, for the page.
type ParcelMail interface {
	Readable(ctx context.Context, principalID string) (bool, error)
}

// ParcelSwitchHook is told when the principal turns Parcels on or off, so the tracker marks the open
// cards as tracking off, or edits them back and runs its silent baseline again. It is a signal that
// must not block: the tracker queues the work on its own goroutine, and a failure is logged and
// never fails the settings action.
type ParcelSwitchHook interface {
	Switched(ctx context.Context, principalID string, enabled bool) error
}

// ParcelsSettings is what the Parcels page shows and changes.
type ParcelsSettings struct {
	Toggle SwitchController
	// Sources lists the principal's Home Assistant sources; nil shows none.
	Sources parcel.Sources
	// Settings keeps the postcode.
	Settings interface {
		LoadSettings(ctx context.Context, principalID string) (parcel.Settings, error)
		SaveSettings(ctx context.Context, settings parcel.Settings) error
	}
	// Mail is optional; without it the page does not say whether mail is readable.
	Mail ParcelMail
	// Hook is optional.
	Hook ParcelSwitchHook
	// Now is the clock of the postcode intake; nil is the wall clock.
	Now func() time.Time
}

// NewParcelsSection is the page that turns Parcels on and off and shows what feeds it. It also takes
// the postcode in the principal's next message after its button is pressed.
func NewParcelsSection(settings ParcelsSettings, logger *slog.Logger, telemetry Telemetry) Section {
	if settings.Now == nil {
		settings.Now = time.Now
	}
	return &parcelsSection{settings: settings, logger: logger, telemetry: telemetryOrNoop(telemetry), asked: make(map[string]time.Time)}
}

type parcelsSection struct {
	settings  ParcelsSettings
	logger    *slog.Logger
	telemetry Telemetry

	mu    sync.Mutex
	asked map[string]time.Time
}

func (p *parcelsSection) prefix() string { return parcelsSectionPrefix }

func (p *parcelsSection) helpTopic() string { return ParcelsPage.HelpTopic }

func (p *parcelsSection) switchPage() switchSection {
	return switchSection{settings: SwitchSettings{Page: ParcelsPage, Toggle: p.settings.Toggle}, logger: p.logger, telemetry: p.telemetry}
}

func (p *parcelsSection) privacy(ctx context.Context, principalID string) ([]string, error) {
	return p.switchPage().privacy(ctx, principalID)
}

func (p *parcelsSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	summary, err := p.switchPage().summary(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	if enabled, err := p.settings.Toggle.Enabled(ctx, principalID); err == nil && enabled {
		sources, err := p.sources(ctx, principalID)
		if err != nil {
			p.telemetry.ContextLogger(ctx, p.logger).WarnContext(ctx, "parcels.sources.failed", "principal_id", principalID, "error", err)
			summary.attention = parcelsSourcesUnknownNote.Format(ctx)
		}
		for _, source := range sources {
			if source.State == parcel.SourceStale || source.State == parcel.SourceMissing {
				summary.attention = parcelsSourcesStale.Format(ctx)
			}
		}
	}
	return summary, nil
}

func (p *parcelsSection) sources(ctx context.Context, principalID string) ([]parcel.SourceInfo, error) {
	if p.settings.Sources == nil {
		return nil, nil
	}
	return p.settings.Sources.List(ctx, principalID)
}

func (p *parcelsSection) render(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := p.settings.Toggle.Enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	page := ParcelsPage
	if !enabled {
		return settingsView{
			lines:   []string{page.OffLine.Format(ctx)},
			buttons: []InlineButton{{Text: page.OnButton.Format(ctx), CallbackData: page.Prefix + ":" + parcelsOnAction}},
		}, nil
	}
	sources, sourcesErr := p.sources(ctx, principalID)
	if sourcesErr != nil {
		p.telemetry.ContextLogger(ctx, p.logger).WarnContext(ctx, "parcels.sources.failed", "principal_id", principalID, "error", sourcesErr)
	}
	saved, err := p.settings.Settings.LoadSettings(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	lines := []string{page.OnLine.Format(ctx), ""}
	switch {
	case sourcesErr != nil:
		lines = append(lines, parcelsSourcesUnknown.Format(ctx))
	case len(sources) == 0:
		lines = append(lines, parcelsNoSources.Format(ctx))
	default:
		lines = append(lines, parcelsSourcesHeader.Format(ctx))
		for _, source := range sources {
			lines = append(lines, parcelsSourceLine.Format(ctx, parcelSettingsText(source.Title), parcelSettingsText(source.Domain), sourceStateWord(ctx, source.State)))
		}
	}
	lines = append(lines, "")
	setButton := parcelsSetButton
	if saved.Postcode == "" {
		lines = append(lines, parcelsNoPostcode.Format(ctx))
	} else {
		if ctx.Value(helpViewKey{}) != nil {
			lines = append(lines, parcelsPostcodeSet.Format(ctx))
		} else {
			lines = append(lines, parcelsPostcodeLine.Format(ctx, saved.Postcode))
		}
		setButton = parcelsChangeButton
	}
	if p.settings.Mail != nil {
		readable, err := p.settings.Mail.Readable(ctx, principalID)
		switch {
		case err != nil:
			lines = append(lines, parcelsMailUnknown.Format(ctx))
		case readable:
			lines = append(lines, parcelsMailReadable.Format(ctx))
		default:
			lines = append(lines, parcelsMailBlocked.Format(ctx))
		}
	}
	view := settingsView{
		lines:   lines,
		buttons: []InlineButton{{Text: page.OffButton.Format(ctx), CallbackData: page.Prefix + ":" + parcelsOffAction}},
	}
	row := []InlineButton{{Text: setButton.Format(ctx), CallbackData: page.Prefix + ":" + parcelsPostcode}}
	if saved.Postcode != "" {
		row = append(row, InlineButton{Text: parcelsClearButton.Format(ctx), CallbackData: page.Prefix + ":" + parcelsClearPostcode})
	}
	view.rows = [][]InlineButton{row}
	return view, nil
}

func sourceStateWord(ctx context.Context, state parcel.SourceState) string {
	switch state {
	case parcel.SourceOK:
		return parcelsSourceOK.Format(ctx)
	case parcel.SourceMissing:
		return parcelsSourceMissing.Format(ctx)
	case parcel.SourceUnknown:
		return parcelsSourceUnknown.Format(ctx)
	}
	return parcelsSourceStale.Format(ctx)
}

// parcelSettingsText is an account's title as a short line that /settings, which reads markup, shows
// as it is.
func parcelSettingsText(text string) string {
	return parcelText(strings.NewReplacer("*", "", "[", "(", "]", ")", "`", "'", "<", "", ">", "").Replace(text), maximumParcelSourceTitleRunes)
}

func (p *parcelsSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	logger := p.telemetry.ContextLogger(ctx, p.logger)
	switch action {
	case parcelsOnAction, parcelsOffAction:
		enabled := action == parcelsOnAction
		was, err := p.settings.Toggle.Enabled(ctx, principalID)
		if err != nil {
			return settingsOutcome{}, err
		}
		if err := p.settings.Toggle.SetEnabled(ctx, principalID, enabled); err != nil {
			return settingsOutcome{}, err
		}
		logger.InfoContext(ctx, ParcelsPage.Event, "principal_id", principalID, "enabled", enabled)
		if !enabled {
			p.forget(principalID)
		}
		if p.settings.Hook != nil && was != enabled {
			if err := p.settings.Hook.Switched(ctx, principalID, enabled); err != nil {
				logger.WarnContext(ctx, "parcels.switch_hook.failed", "principal_id", principalID, "enabled", enabled, "error", err)
				return settingsOutcome{note: parcelsCatchUpNote.Format(ctx)}, nil
			}
		}
		return settingsOutcome{}, nil
	case parcelsPostcode, parcelsClearPostcode:
		if enabled, err := p.settings.Toggle.Enabled(ctx, principalID); err != nil {
			return settingsOutcome{}, err
		} else if !enabled {
			return settingsOutcome{}, errUnknownSettingsAction
		}
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
	if action == parcelsClearPostcode {
		if err := p.savePostcode(ctx, principalID, ""); err != nil {
			return settingsOutcome{}, err
		}
		p.forget(principalID)
		return settingsOutcome{note: parcelsPostcodeGone.Format(ctx)}, nil
	}
	p.mu.Lock()
	p.asked[principalID] = p.settings.Now().Add(parcelsIntakeLifetime)
	p.mu.Unlock()
	return settingsOutcome{message: parcelsPostcodeAsk.Format(ctx, int(parcelsIntakeLifetime.Minutes()))}, nil
}

func (p *parcelsSection) savePostcode(ctx context.Context, principalID, postcode string) error {
	saved, err := p.settings.Settings.LoadSettings(ctx, principalID)
	if err != nil {
		return err
	}
	saved.PrincipalID, saved.Postcode = principalID, postcode
	return p.settings.Settings.SaveSettings(ctx, saved)
}

func (p *parcelsSection) forget(principalID string) {
	p.mu.Lock()
	delete(p.asked, principalID)
	p.mu.Unlock()
}

func (p *parcelsSection) awaiting(principalID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, asked := p.asked[principalID]
	if asked && !p.settings.Now().Before(until) {
		delete(p.asked, principalID)
		return false
	}
	return asked
}

// claims is false: a postcode has no shape another section's guard could mistake for a key.
func (p *parcelsSection) claims(string, string) bool { return false }

func (p *parcelsSection) yield(principalID string) { p.forget(principalID) }

// guard has no secret to keep out of the model.
func (p *parcelsSection) guard(context.Context, string, string) interception { return interception{} }

// intercept takes the message after Set postcode as the postcode, and tells the user when it is
// not one. The text is never logged.
func (p *parcelsSection) intercept(ctx context.Context, principalID, text string) (interception, error) {
	if !p.awaiting(principalID) {
		return interception{}, nil
	}
	postcode := strings.Join(strings.Fields(text), " ")
	if !postcodePattern.MatchString(postcode) || !strings.ContainsAny(postcode, "0123456789") {
		p.forget(principalID)
		return interception{handled: true, reply: parcelsPostcodeBad.Format(ctx), reason: "parcels_postcode_refused"}, nil
	}
	if err := p.savePostcode(ctx, principalID, strings.ToUpper(postcode)); err != nil {
		p.telemetry.ContextLogger(ctx, p.logger).WarnContext(ctx, "parcels.postcode.save_failed", "principal_id", principalID, "error", err)
		return interception{handled: true, reply: parcelsPostcodeFailed.Format(ctx), reason: "parcels_postcode_failed"}, nil
	}
	p.forget(principalID)
	return interception{handled: true, reply: parcelsPostcodeSaved.Format(ctx), reason: "parcels_postcode_saved"}, nil
}
