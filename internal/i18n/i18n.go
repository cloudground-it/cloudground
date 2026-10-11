// Package i18n holds the few strings the panel writes for people outside the
// web interface (alert webhooks), the names of the task kinds (tasks.go,
// shared by the interface, the alerts and the webhook), and the agent's
// messages that carry a key
// (agentrpc.Error.Key, shown translated in a task's error), in the five
// interface languages. The web
// interface has its own catalogue in ui/i18n/locales; terms follow the same
// glossary (ui/i18n/GLOSSARY.md).
package i18n

import (
	"fmt"
	"strings"
)

// Default is the language used when none is chosen or the one asked for is
// not in the catalogue.
const Default = "it"

// catalogue maps a language to its messages. Every language has every key,
// with the same verbs in the same order (TestCatalogueComplete).
var catalogue = map[string]map[string]string{
	"it": {
		"alert.level.error":       "ERRORE",
		"alert.level.warning":     "AVVISO",
		"alert.header":            "CloudGround %s: %s",
		"alert.headerSite":        "CloudGround %s su %s: %s",
		"alert.certRenewal":       "rinnovo del certificato non riuscito: %s",
		"alert.certExpiresOne":    "il certificato scade tra %d giorno",
		"alert.certExpires":       "il certificato scade tra %d giorni",
		"alert.taskFailed":        "operazione non riuscita (%s): %s",
		"alert.serviceFailed":     "%s è in errore: %s",
		"alert.serviceStopped":    "%s è fermo: %s",
		"alert.driftOne":          "%d file gestito è stato modificato fuori dal pannello",
		"alert.drift":             "%d file gestiti sono stati modificati fuori dal pannello",
		"alert.deployDrift":       "il sito serve il rilascio %s, non quello che il pannello aveva registrato come attivo (%s)",
		"alert.deployUnverified":  "il rilascio %s è attivo senza la verifica dopo la promozione (è stata annullata)",
		"alert.deployRegressed":   "il rilascio %s è peggiorato dopo la promozione e il rollback automatico non è riuscito",
		"alert.tokenGuessing":     "%s token del connettore non validi dal %s: qualcuno potrebbe tentare di indovinare i token dei siti",
		"alert.siteDown":          "il sito non risponde dal suo indirizzo pubblico: %s",
		"alert.siteCPU":           "il sito ha usato il %s%% della sua CPU per almeno %s minuti",
		"alert.siteMemory":        "il sito ha usato il %s%% del suo limite di memoria per almeno %s minuti",
		"alert.siteDisk":          "il sito occupa %s MB di disco, oltre la soglia di %s MB",
		"alert.vulnsOne":          "%d vulnerabilità nota in WordPress, nei plugin o nei temi: %s",
		"alert.vulns":             "%d vulnerabilità note in WordPress, nei plugin o nei temi: %s",
		"migration.tooLargeRatio": "L'archivio si espande oltre %s, %s volte la sua dimensione compressa, il limite. Un amministratore può alzare il limite di espansione nel modulo di migrazione, fino a %s volte.",
		"migration.tooLargeSize":  "L'archivio si espande oltre %s, il massimo che una migrazione può estrarre.",
		"migration.tooLargeDisk":  "L'archivio si espande oltre %s, lo spazio libero su disco a disposizione (%s restano sempre liberi). Libera spazio su disco e riprova.",
	},
	"en": {
		"alert.level.error":       "ERROR",
		"alert.level.warning":     "WARNING",
		"alert.header":            "CloudGround %s: %s",
		"alert.headerSite":        "CloudGround %s on %s: %s",
		"alert.certRenewal":       "certificate renewal failed: %s",
		"alert.certExpiresOne":    "certificate expires in %d day",
		"alert.certExpires":       "certificate expires in %d days",
		"alert.taskFailed":        "%s failed: %s",
		"alert.serviceFailed":     "%s has failed: %s",
		"alert.serviceStopped":    "%s is stopped: %s",
		"alert.driftOne":          "%d managed file changed outside the panel",
		"alert.drift":             "%d managed files changed outside the panel",
		"alert.deployDrift":       "the site serves release %s, not the one the panel had recorded as live (%s)",
		"alert.deployUnverified":  "release %s is live without its post-promotion verification (it was canceled)",
		"alert.deployRegressed":   "release %s regressed after its promotion and the automatic rollback failed",
		"alert.tokenGuessing":     "%s invalid connector tokens since %s: something may be guessing the sites' tokens",
		"alert.siteDown":          "the site does not answer from its public address: %s",
		"alert.siteCPU":           "the site has used %s%% of its CPU for at least %s minutes",
		"alert.siteMemory":        "the site has used %s%% of its memory limit for at least %s minutes",
		"alert.siteDisk":          "the site uses %s MB of disk, over its threshold of %s MB",
		"alert.vulnsOne":          "%d known vulnerability in WordPress, its plugins or themes: %s",
		"alert.vulns":             "%d known vulnerabilities in WordPress, its plugins or themes: %s",
		"migration.tooLargeRatio": "The archive expands to more than %s, %s times its compressed size, the limit. An administrator can raise the expansion limit in the migration form, up to %s times.",
		"migration.tooLargeSize":  "The archive expands to more than %s, the most a migration may unpack.",
		"migration.tooLargeDisk":  "The archive expands to more than %s, the free disk space for it (%s is kept free). Free some disk space and try again.",
	},
	"fr": {
		"alert.level.error":       "ERREUR",
		"alert.level.warning":     "AVERTISSEMENT",
		"alert.header":            "CloudGround %s\u00a0: %s",
		"alert.headerSite":        "CloudGround %s sur %s\u00a0: %s",
		"alert.certRenewal":       "échec du renouvellement du certificat\u00a0: %s",
		"alert.certExpiresOne":    "le certificat expire dans %d jour",
		"alert.certExpires":       "le certificat expire dans %d jours",
		"alert.taskFailed":        "échec de l’opération (%s)\u00a0: %s",
		"alert.serviceFailed":     "%s est en échec\u00a0: %s",
		"alert.serviceStopped":    "%s est arrêté\u00a0: %s",
		"alert.driftOne":          "%d fichier géré a été modifié en dehors du panneau",
		"alert.drift":             "%d fichiers gérés ont été modifiés en dehors du panneau",
		"alert.deployDrift":       "le site sert la release %s, pas celle que le panneau avait enregistrée comme en ligne (%s)",
		"alert.deployUnverified":  "la release %s est en ligne sans sa vérification après promotion (elle a été annulée)",
		"alert.deployRegressed":   "la release %s a régressé après sa promotion et le rollback automatique a échoué",
		"alert.tokenGuessing":     "%s jetons de connecteur invalides depuis %s\u00a0: quelque chose tente peut-être de deviner les jetons des sites",
		"alert.siteDown":          "le site ne répond pas depuis son adresse publique\u00a0: %s",
		"alert.siteCPU":           "le site a utilisé %s\u00a0%% de son CPU pendant au moins %s minutes",
		"alert.siteMemory":        "le site a utilisé %s\u00a0%% de sa limite de mémoire pendant au moins %s minutes",
		"alert.siteDisk":          "le site occupe %s\u00a0Mo de disque, au-delà de son seuil de %s\u00a0Mo",
		"alert.vulnsOne":          "%d vulnérabilité connue dans WordPress, ses extensions ou ses thèmes\u00a0: %s",
		"alert.vulns":             "%d vulnérabilités connues dans WordPress, ses extensions ou ses thèmes\u00a0: %s",
		"migration.tooLargeRatio": "L’archive dépasse %s une fois décompressée, soit %s fois sa taille compressée, la limite. Un administrateur peut relever la limite d’expansion dans le formulaire de migration, jusqu’à %s fois.",
		"migration.tooLargeSize":  "L’archive dépasse %s une fois décompressée, le maximum qu’une migration peut extraire.",
		"migration.tooLargeDisk":  "L’archive dépasse %s une fois décompressée, l’espace disque libre disponible (%s restent toujours libres). Libérez de l’espace disque et réessayez.",
	},
	"es": {
		"alert.level.error":       "ERROR",
		"alert.level.warning":     "AVISO",
		"alert.header":            "CloudGround %s: %s",
		"alert.headerSite":        "CloudGround %s en %s: %s",
		"alert.certRenewal":       "no se pudo renovar el certificado: %s",
		"alert.certExpiresOne":    "el certificado caduca en %d día",
		"alert.certExpires":       "el certificado caduca en %d días",
		"alert.taskFailed":        "la operación ha fallado (%s): %s",
		"alert.serviceFailed":     "%s ha fallado: %s",
		"alert.serviceStopped":    "%s está detenido: %s",
		"alert.driftOne":          "%d archivo gestionado se ha modificado fuera del panel",
		"alert.drift":             "%d archivos gestionados se han modificado fuera del panel",
		"alert.deployDrift":       "el sitio sirve la release %s, no la que el panel había registrado como en producción (%s)",
		"alert.deployUnverified":  "la release %s está en producción sin su verificación tras la promoción (se canceló)",
		"alert.deployRegressed":   "la release %s empeoró tras su promoción y el rollback automático falló",
		"alert.tokenGuessing":     "%s tokens de conector no válidos desde %s: puede que alguien intente adivinar los tokens de los sitios",
		"alert.siteDown":          "el sitio no responde desde su dirección pública: %s",
		"alert.siteCPU":           "el sitio ha usado el %s%% de su CPU durante al menos %s minutos",
		"alert.siteMemory":        "el sitio ha usado el %s%% de su límite de memoria durante al menos %s minutos",
		"alert.siteDisk":          "el sitio ocupa %s MB de disco, por encima de su umbral de %s MB",
		"alert.vulnsOne":          "%d vulnerabilidad conocida en WordPress, sus plugins o temas: %s",
		"alert.vulns":             "%d vulnerabilidades conocidas en WordPress, sus plugins o temas: %s",
		"migration.tooLargeRatio": "El archivo se expande a más de %s, %s veces su tamaño comprimido, el límite. Un administrador puede aumentar el límite de expansión en el formulario de migración, hasta %s veces.",
		"migration.tooLargeSize":  "El archivo se expande a más de %s, el máximo que una migración puede extraer.",
		"migration.tooLargeDisk":  "El archivo se expande a más de %s, el espacio libre en disco disponible (%s se mantienen libres). Libera espacio en disco y vuelve a intentarlo.",
	},
	"de": {
		"alert.level.error":       "FEHLER",
		"alert.level.warning":     "WARNUNG",
		"alert.header":            "CloudGround %s: %s",
		"alert.headerSite":        "CloudGround %s bei %s: %s",
		"alert.certRenewal":       "Zertifikatserneuerung fehlgeschlagen: %s",
		"alert.certExpiresOne":    "Zertifikat läuft in %d Tag ab",
		"alert.certExpires":       "Zertifikat läuft in %d Tagen ab",
		"alert.taskFailed":        "Vorgang fehlgeschlagen (%s): %s",
		"alert.serviceFailed":     "%s ist fehlgeschlagen: %s",
		"alert.serviceStopped":    "%s ist gestoppt: %s",
		"alert.driftOne":          "%d verwaltete Datei wurde außerhalb des Panels geändert",
		"alert.drift":             "%d verwaltete Dateien wurden außerhalb des Panels geändert",
		"alert.deployDrift":       "die Website liefert Release %s aus, nicht das, das das Panel als live erfasst hatte (%s)",
		"alert.deployUnverified":  "Release %s ist live ohne die Prüfung nach der Freigabe (sie wurde abgebrochen)",
		"alert.deployRegressed":   "Release %s hat sich nach der Freigabe verschlechtert und der automatische Rollback ist fehlgeschlagen",
		"alert.tokenGuessing":     "%s ungültige Connector-Tokens seit %s: möglicherweise versucht jemand, die Tokens der Websites zu erraten",
		"alert.siteDown":          "die Website antwortet nicht über ihre öffentliche Adresse: %s",
		"alert.siteCPU":           "die Website hat %s %% ihrer CPU mindestens %s Minuten lang genutzt",
		"alert.siteMemory":        "die Website hat %s %% ihres Speicherlimits mindestens %s Minuten lang genutzt",
		"alert.siteDisk":          "die Website belegt %s MB Speicherplatz, mehr als ihr Schwellenwert von %s MB",
		"alert.vulnsOne":          "%d bekannte Sicherheitslücke in WordPress, seinen Plugins oder Themes: %s",
		"alert.vulns":             "%d bekannte Sicherheitslücken in WordPress, seinen Plugins oder Themes: %s",
		"migration.tooLargeRatio": "Das Archiv entpackt sich auf mehr als %s, das %s-Fache seiner komprimierten Größe und damit das Limit. Ein Administrator kann das Expansionslimit im Migrationsformular erhöhen, bis zum %s-Fachen.",
		"migration.tooLargeSize":  "Das Archiv entpackt sich auf mehr als %s, das Maximum, das eine Migration entpacken darf.",
		"migration.tooLargeDisk":  "Das Archiv entpackt sich auf mehr als %s, den freien Speicherplatz dafür (%s bleiben immer frei). Gib Speicherplatz frei und versuche es erneut.",
	},
}

// Supported reports whether lang has a catalogue.
func Supported(lang string) bool {
	_, ok := catalogue[lang]
	return ok
}

// T formats the message key in lang, falling back to Default for an unknown
// language and to English for a key missing in lang. An unknown key is
// returned as it is.
func T(lang, key string, args ...any) string {
	msgs, ok := catalogue[lang]
	if !ok {
		msgs = catalogue[Default]
	}
	f, ok := msgs[key]
	if !ok {
		if f, ok = catalogue["en"][key]; !ok {
			return key
		}
	}
	return fmt.Sprintf(f, args...)
}

// N is T for a message with a count: key+"One" when n is 1, key otherwise
// (all five languages have one singular and one plural form). n is passed
// as the first argument.
func N(lang, key string, n int, args ...any) string {
	if n == 1 {
		key += "One"
	}
	return T(lang, key, append([]any{n}, args...)...)
}

// FromAcceptLanguage is the first language of an Accept-Language header
// that has a catalogue ("de-DE,de;q=0.9" gives "de"), or "" if none has.
// Quality values are not weighed: browsers and the panel's interface list
// languages in order of preference.
func FromAcceptLanguage(header string) string {
	for _, part := range strings.Split(header, ",") {
		tag, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		primary, _, _ := strings.Cut(tag, "-")
		if l := strings.ToLower(primary); Supported(l) {
			return l
		}
	}
	return ""
}
