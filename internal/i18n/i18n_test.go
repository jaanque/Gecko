package i18n

import "testing"

func TestNormalizeAndClientStrings(t *testing.T) {
	if got := Normalize("ca_ES.UTF-8", LangEN); got != LangCA {
		t.Fatalf("expected ca, got %q", got)
	}
	if got := Normalize("es-ES", LangCA); got != LangES {
		t.Fatalf("expected es, got %q", got)
	}
	if got := Normalize("en_US.UTF-8", LangCA); got != LangEN {
		t.Fatalf("expected en, got %q", got)
	}
	if got := Normalize("fr_FR", LangEN); got != LangEN {
		t.Fatalf("expected fallback en, got %q", got)
	}

	for _, lang := range []Lang{LangCA, LangES, LangEN} {
		s := Client(lang)
		if s.WindowTitle == "" || s.Allow == "" || s.Deny == "" || s.PromptMessage == "" {
			t.Fatalf("incomplete translations for %q: %+v", lang, s)
		}
	}
}
