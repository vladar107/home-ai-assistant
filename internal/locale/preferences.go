package locale

import "context"

// Preferences keeps the language each principal chose. A principal without a choice has English.
type Preferences interface {
	LoadLanguage(ctx context.Context, principalID string) (language Language, chosen bool, err error)
	SaveLanguage(ctx context.Context, principalID string, language Language) error
	// ChosenLanguages lists every principal's choice, for work that starts without one.
	ChosenLanguages(ctx context.Context) (map[string]Language, error)
}

// ForPrincipal returns a context that carries the principal's chosen language. Without a choice,
// or without preferences, the context is returned as it is and reads as English; a store failure
// is returned with the unchanged context so the caller can log it and go on in English.
func ForPrincipal(ctx context.Context, preferences Preferences, principalID string) (context.Context, error) {
	if preferences == nil || principalID == "" {
		return ctx, nil
	}
	language, chosen, err := preferences.LoadLanguage(ctx, principalID)
	if err != nil || !chosen {
		return ctx, err
	}
	return With(ctx, language), nil
}
