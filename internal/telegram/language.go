package telegram

import (
	"context"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/locale"
)

const languageSectionPrefix = "language"

// activeChats finds the private chat of a principal, whose slash menu follows their language.
type activeChats interface {
	ResolveActiveChat(ctx context.Context, principalID string) (int64, bool, error)
}

// languageChoices are the buttons of the Language page, each written in its own language so a
// user who cannot read the current one can still find theirs.
var languageChoices = []struct {
	language locale.Language
	label    string
}{
	{locale.English, "English"},
	{locale.Russian, "Русский"},
}

var languagePageLine = locale.T(
	"Interface language: English. My replies, buttons, and menus are written in it; a conversation goes on in whatever language you write.",
	"Язык интерфейса: русский. На нём написаны мои ответы, кнопки и меню; разговор идёт на том языке, на котором ты пишешь.",
)

var languageState = locale.T("English", "русский")

// languageSection is the Language page, at the top of /settings. The service sets its own
// pointer, because choosing a language also sets the chat's slash menu.
type languageSection struct {
	service *Service
}

func withLanguageSection(sections []Section, language *languageSection) []Section {
	if language == nil {
		return sections
	}
	return append([]Section{language}, sections...)
}

func (l *languageSection) prefix() string { return languageSectionPrefix }

func (l *languageSection) summary(ctx context.Context, _ string) (settingsSummary, error) {
	return settingsSummary{name: locale.T("Language", "Язык").Format(ctx), state: languageState.Format(ctx)}, nil
}

func (l *languageSection) render(ctx context.Context, _ string) (settingsView, error) {
	buttons := make([]InlineButton, 0, len(languageChoices))
	for _, choice := range languageChoices {
		buttons = append(buttons, InlineButton{Text: choice.label, CallbackData: languageSectionPrefix + ":" + choice.language.String()})
	}
	return settingsView{lines: []string{languagePageLine.Format(ctx)}, buttons: buttons}, nil
}

func (l *languageSection) act(ctx context.Context, principalID string, chatID int64, action string) (settingsOutcome, error) {
	language, ok := locale.Parse(action)
	if !ok {
		return settingsOutcome{}, errUnknownSettingsAction
	}
	if err := l.service.languages.SaveLanguage(ctx, principalID, language); err != nil {
		return settingsOutcome{}, err
	}
	l.service.telemetry.ContextLogger(ctx, l.service.logger).InfoContext(ctx, "language.changed", "principal_id", principalID, "language", language.String())
	l.service.setChatMenu(ctx, principalID, chatID, language)
	return settingsOutcome{}, nil
}

// withLanguage puts the principal's chosen language in the context, as the update's sender was
// just authorized. A store failure leaves English and is logged.
func (s *Service) withLanguage(ctx context.Context, principalID string) context.Context {
	withLanguage, err := locale.ForPrincipal(ctx, s.languages, principalID)
	if err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.language.load_failed", "principal_id", principalID, "error", err)
	}
	return withLanguage
}

// setChatMenu sets the slash menu of one chat in a language; English removes the chat's own list,
// so the chat uses the shared English one. A failed registration is logged and leaves the old menu.
func (s *Service) setChatMenu(ctx context.Context, principalID string, chatID int64, language locale.Language) {
	scope := ChatCommandScope(chatID)
	var err error
	if language == locale.English {
		err = s.client.DeleteMyCommands(ctx, scope)
	} else {
		err = s.client.SetMyCommands(ctx, scope, s.botCommands(language))
	}
	if err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.commands.register_failed", slog.String("scope", "chat"), "principal_id", principalID, "chat_id", chatID, "language", language.String(), "error", err)
	}
}

// ChatMenuChanged sets the menu of a principal's chat after their language changed other than by
// their own press, such as a restore.
func (s *Service) ChatMenuChanged(ctx context.Context, principalID string) {
	if s.languages == nil || s.chats == nil {
		return
	}
	language, _, err := s.languages.LoadLanguage(ctx, principalID)
	if err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.language.load_failed", "principal_id", principalID, "error", err)
		return
	}
	if chatID, found, err := s.chats.ResolveActiveChat(ctx, principalID); err == nil && found {
		s.setChatMenu(ctx, principalID, chatID, language)
	}
}

// registerChosenMenus gives each principal who chose a language other than English their menu in
// their chat; the shared scopes keep the English list.
func (s *Service) registerChosenMenus(ctx context.Context) {
	if s.languages == nil || s.chats == nil {
		return
	}
	chosen, err := s.languages.ChosenLanguages(ctx)
	if err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.language.load_failed", "error", err)
		return
	}
	for principalID, language := range chosen {
		if language == locale.English {
			continue
		}
		chatID, found, err := s.chats.ResolveActiveChat(ctx, principalID)
		if err != nil {
			s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.language.chat_failed", "principal_id", principalID, "error", err)
			continue
		}
		if found {
			s.setChatMenu(ctx, principalID, chatID, language)
		}
	}
}
