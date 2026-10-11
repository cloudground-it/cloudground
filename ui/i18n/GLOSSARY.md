# CloudGround — translation glossary

The interface ships in five languages: Italian (`it`, the default), English
(`en`), French (`fr`), Spanish (`es`) and German (`de`). `en.json` is the
reference for keys and placeholders; `it.json` is the reference for tone.
Alert webhooks use a small Go catalogue (`internal/i18n`) that follows the
same choices.

There are two sets of files, each with one file per language: `locales/`
(the interface) and `design/` (the panel design's `cg.` strings: titles with
their accent word, eyebrows, table heads), merged by `i18n.config.ts`.

`npm test` (in `ui/`) fails, for either set, when a locale has a key missing
or extra compared with that set's `en.json`, a placeholder that differs, a key containing a dot, or a
message that the i18n compiler would misread (see *Syntax* below).

## Tone

| | Address | Why |
| --- | --- | --- |
| it | **tu** (informal) | The project's choice. |
| en | neutral, "you" | — |
| fr | **vous** | French developer and hosting tools (GitHub, Scaleway, OVHcloud, o2switch) use *vous*; *tu* reads as a consumer app. |
| es | **tú** | Modern Spanish developer tools (GitHub, Hostinger, Stripe, Raiola) use *tú*; *usted* reads as a bank. |
| de | **du** | GitHub, Stripe, Figma, Raidboxes and most modern German developer products use *du*; *Sie* reads as a traditional host (and is longer). |

Buttons and menu items are infinitives or nouns, never imperatives in the
address form: fr *Enregistrer*, es *Guardar*, de *Speichern*. Messages are
short and factual, like the English: no exclamation marks, no "please".
Capitalise only the first word (German nouns, of course, keep theirs).

## Terms of art

Product names are never translated: **CloudGround, Safe Push, Performance
Guard, Velocity Engine, Quarry, cgctl**, and third-party names (WordPress,
WooCommerce, Laravel, Let's Encrypt, Cloudflare, restic, Slack…). WordPress's
own terms stay in English too: *transients*, *GUID*.

Technical identifiers stay as they are in every language: **SFTP, SSH, PHP,
PHP-FPM, OPcache, APCu, HSTS, HTTPS, TTL, SQL, DNS, URL, IP, CPU, 2FA, QR,
PEM, wp-admin, cron** (the syntax), file names and paths.

| English | it | fr | es | de |
| --- | --- | --- | --- | --- |
| cache (noun) | cache (f.) | cache (m.) | caché (f.) | Cache (m.) |
| page cache | cache di pagina | cache de pages | caché de página | Seiten-Cache |
| object cache | object cache | cache d'objets | caché de objetos | Objekt-Cache |
| purge (cache) | svuotare | vider | purgar | leeren |
| cache warming | riscaldamento cache | préchauffage du cache | precarga de la caché | Cache-Vorwärmen |
| staging | staging | staging (m.), *environnement de staging* | staging (m.), *entorno de staging* | Staging (n.) |
| production | produzione | production | producción | Produktion / Live-Website |
| release (noun) | rilascio | release (f.) | release (f.) | Release (n.) |
| deploy / publish | pubblicare | publier | publicar | veröffentlichen |
| rollback | rollback | rollback (m.) | rollback (m.) | Rollback (m.) |
| backup | backup | sauvegarde | copia de seguridad | Backup (n.) |
| restore | ripristino | restauration | restauración | Wiederherstellung |
| restore test | ripristino di prova | test de restauration | prueba de restauración | Wiederherstellungstest |
| snapshot | snapshot | snapshot (m.) | snapshot (m.) | Snapshot (m.) |
| scheduled task / cron job | attività pianificata | tâche planifiée (cron) | tarea programada (cron) | Cronjob |
| task (background) | attività | opération | operación | Vorgang |
| site | sito | site | sitio | Website (f.) |
| domain | dominio | domaine | dominio | Domain (f.) |
| panel | pannello | panneau | panel | Panel (n.) |
| dashboard | dashboard | tableau de bord | panel principal | Dashboard (n.) |
| alert | avviso | alerte | alerta | Warnung |
| logs | log | journaux | registros | Logs |
| audit log | registro attività | journal d'audit | registro de auditoría | Audit-Log |
| firewall | firewall | pare-feu | firewall | Firewall |
| block (an address) / blocking | bloccare / blocchi | bloquer / blocages | bloquear / bloqueos | sperren / Sperren |
| always allowed (allowlist) | sempre consentiti | toujours autorisées | siempre permitidas | immer erlaubt |
| sign-in (failed) | accesso (fallito) | connexion (échouée) | inicio de sesión (fallido) | Anmeldung (fehlgeschlagen) |
| rule | regola | règle | regla | Regel |
| certificate | certificato | certificat | certificado | Zertifikat |
| wildcard | wildcard | wildcard | wildcard | Wildcard |
| webhook | webhook | webhook | webhook | Webhook |
| repository | repository | dépôt | repositorio | Repository |
| upload / download | caricare / scaricare | téléverser (envoyer) / télécharger | subir / descargar | hochladen / herunterladen |
| folder | cartella | dossier | carpeta | Ordner |
| service | servizio | service | servicio | Dienst |
| user (panel) / system user | utente / utente di sistema | utilisateur / utilisateur système | usuario / usuario del sistema | Benutzer / Systembenutzer |
| administrator / operator / read-only | amministratore / operatore / sola lettura | administrateur / opérateur / lecture seule | administrador / operador / solo lectura | Administrator / Operator / Nur Lesen |
| two-step verification | verifica in due passaggi | validation en deux étapes | verificación en dos pasos | Zwei-Faktor-Authentifizierung |
| session | sessione | session | sesión | Sitzung |
| sign in / sign out | accedi / esci | se connecter / se déconnecter | iniciar sesión / cerrar sesión | anmelden / abmelden |
| drift (config changed outside the panel) | modifica esterna | modification externe | cambio externo | externe Änderung |
| headroom / in use | in uso | utilisé | en uso | belegt |

Where a language has two common words, the table's is the one to use
everywhere, so the same thing is never called two names.

## Formatting

Never hard-code a format in a message: dates, times, numbers and sizes come
from `ui/app/utils/format.ts`, which follows the active locale through
`Intl` (`it-IT`, `en-US`, `fr-FR`, `es-ES`, `de-DE`).

| | it | en | fr | es | de |
| --- | --- | --- | --- | --- | --- |
| date + time (`medium`/`short`) | 9 ott 2026, 14:05 | Oct 9, 2026, 2:05 PM | 9 oct. 2026, 14:05 | 9 oct 2026, 14:05 | 09.10.2026, 14:05 |
| decimal | 1,5 | 1.5 | 1,5 | 1,5 | 1,5 |
| sizes | KB MB GB | KB MB GB | **Ko Mo Go** | KB MB GB | KB MB GB |
| percent in text | 12% | 12% | 12 % | 12 % | 12 % |
| quotes | « » | " " | « » | « » | „ " |

- Sizes are binary multiples (1 KB = 1024 B), as everywhere in the panel;
  only the unit letters change (French uses *octet*: o, Ko, Mo, Go, To).
- French puts a non-breaking space (U+00A0) before `:` `;` `?` `!` and
  inside « ». The other languages do not.
- Times of day in prose: it "alle 3", en "at 3am", fr "à 3 h", es "a las
  3:00", de "um 3 Uhr".

## Syntax

The messages are compiled by vue-i18n:

- Placeholders are `{name}`. Keep every placeholder of the English message,
  spelled the same; their order may change.
- `|` separates plural forms and `@` starts a linked message: do not use
  either as literal text. The current messages have no plural forms.
- `$` in the `editor.phrases` keys is CodeMirror's own placeholder: keep it.
- Keys never contain dots (the test refuses them).
- Apostrophes: use the typographic ’ in French and Italian prose; the plain
  `'` is also safe.
