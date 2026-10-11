package api

import (
	"net/http"

	"github.com/cloudground-it/cloudground/internal/i18n"
	"github.com/cloudground-it/cloudground/internal/jobs"
)

// A task's error that carries a translation key (an agent refusal such as a
// migration's "too large") is stored as the key, its values and the English
// text; it is written in the reader's language when it is read, not in the
// language of whoever started the task.

// readerLanguage is the language a response to r is written in: the
// interface's (Accept-Language), else the caller's saved one, else the
// default. A token caller has a user too, so it gets that user's locale.
func readerLanguage(r *http.Request, c *call) string {
	if lang := i18n.FromAcceptLanguage(r.Header.Get("Accept-Language")); lang != "" {
		return lang
	}
	if c != nil && i18n.Supported(c.User.Locale) {
		return c.User.Locale
	}
	return i18n.Default
}

// localizeTask writes t's error in lang when it has a key this build knows;
// otherwise the stored English stays.
func localizeTask(t *jobs.Task, lang string) {
	if msg, ok := localized(lang, t.ErrorKey, t.ErrorArgs); ok {
		t.Error = msg
	}
}

// localized is key formatted with args in lang, and whether the catalogue
// has key.
func localized(lang, key string, args []string) (string, bool) {
	if key == "" {
		return "", false
	}
	vals := make([]any, len(args))
	for i, a := range args {
		vals[i] = a
	}
	msg := i18n.T(lang, key, vals...)
	return msg, msg != key
}
