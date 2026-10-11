import it from './design/it.json'
import en from './design/en.json'
import fr from './design/fr.json'
import es from './design/es.json'
import de from './design/de.json'
import uxIt from './ux/it.json'
import uxEn from './ux/en.json'
import uxFr from './ux/fr.json'
import uxEs from './ux/es.json'
import uxDe from './ux/de.json'

/*
 * The strings of the panel design (titles with their accent word, HUD
 * eyebrows, table heads) live under `cg.` in i18n/design/; those of the
 * layout system (page descriptions, section headers, the guided backup
 * destination) under `ux.` in i18n/ux/. Both are merged with each locale
 * file as it loads. Every locale has its own file in each set (npm test
 * checks them against en.json); a missing key falls back to English.
 */
export default defineI18nConfig(() => ({
  fallbackLocale: 'en',
  fallbackWarn: false,
  messages: {
    it: { ...it, ...uxIt },
    en: { ...en, ...uxEn },
    fr: { ...fr, ...uxFr },
    es: { ...es, ...uxEs },
    de: { ...de, ...uxDe }
  }
}))
