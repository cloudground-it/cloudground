package i18n

import (
	"regexp"
	"slices"
	"testing"
)

var verb = regexp.MustCompile(`%[a-z]`)

// Every language carries every English key, with the same verbs in the same
// order, so no notification falls back to English or prints %!d(MISSING).
func TestCatalogueComplete(t *testing.T) {
	for _, lang := range []string{"it", "en", "fr", "es", "de"} {
		msgs, ok := catalogue[lang]
		if !ok {
			t.Fatalf("no catalogue for %s", lang)
		}
		if len(msgs) != len(catalogue["en"]) {
			t.Errorf("%s has %d messages, en has %d", lang, len(msgs), len(catalogue["en"]))
		}
		for key, en := range catalogue["en"] {
			m, ok := msgs[key]
			if !ok {
				t.Errorf("%s: missing %s", lang, key)
				continue
			}
			if !slices.Equal(verb.FindAllString(m, -1), verb.FindAllString(en, -1)) {
				t.Errorf("%s: %s has verbs %v, en has %v", lang, key, verb.FindAllString(m, -1), verb.FindAllString(en, -1))
			}
		}
	}
}

func TestT(t *testing.T) {
	if got := N("de", "alert.certExpires", 3); got != "Zertifikat läuft in 3 Tagen ab" {
		t.Errorf("de plural = %q", got)
	}
	if got := N("fr", "alert.certExpires", 1); got != "le certificat expire dans 1 jour" {
		t.Errorf("fr singular = %q", got)
	}
	if got := T("xx", "alert.level.error"); got != "ERRORE" {
		t.Errorf("unknown language should fall back to Italian, got %q", got)
	}
	if got := T("en", "no.such.key"); got != "no.such.key" {
		t.Errorf("unknown key = %q", got)
	}
}

func TestFromAcceptLanguage(t *testing.T) {
	for header, want := range map[string]string{
		"de":                      "de",
		"de-DE,de;q=0.9,en;q=0.8": "de",
		"pt-BR, fr-CA;q=0.7":      "fr",
		"ES":                      "es",
		"pt, ja":                  "",
		"":                        "",
		"*":                       "",
	} {
		if got := FromAcceptLanguage(header); got != want {
			t.Errorf("FromAcceptLanguage(%q) = %q, want %q", header, got, want)
		}
	}
}
