package sqlite

import (
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/locale"
)

func TestLanguageChoiceIsPerPrincipalAndEnglishWithoutOne(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	if language, chosen, err := store.LoadLanguage(ctx, "alice"); err != nil || chosen || language != locale.English {
		t.Fatalf("language before a choice = %v %v %v", language, chosen, err)
	}
	if err := store.SaveLanguage(ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	if language, chosen, err := store.LoadLanguage(ctx, "alice"); err != nil || !chosen || language != locale.Russian {
		t.Fatalf("alice's language = %v %v %v", language, chosen, err)
	}
	if _, chosen, err := store.LoadLanguage(ctx, "bob"); err != nil || chosen {
		t.Fatalf("bob chose %v, err %v", chosen, err)
	}

	if err := store.SaveLanguage(ctx, "alice", locale.English); err != nil {
		t.Fatal(err)
	}
	if language, chosen, err := store.LoadLanguage(ctx, "alice"); err != nil || !chosen || language != locale.English {
		t.Fatalf("alice's language after choosing English = %v %v %v", language, chosen, err)
	}
	if err := store.SaveLanguage(ctx, "bob", locale.Russian); err != nil {
		t.Fatal(err)
	}
	chosen, err := store.ChosenLanguages(ctx)
	if err != nil || len(chosen) != 2 || chosen["alice"] != locale.English || chosen["bob"] != locale.Russian {
		t.Fatalf("chosen languages = %v %v", chosen, err)
	}
}

func TestLanguageChoiceGoesWithItsPrincipal(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	if err := store.SaveLanguage(ctx, "bob", locale.Russian); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM principals WHERE id = 'bob'`); err != nil {
		t.Fatal(err)
	}
	if _, chosen, err := store.LoadLanguage(ctx, "bob"); err != nil || chosen {
		t.Fatalf("a deleted principal still has a language: %v %v", chosen, err)
	}
}
