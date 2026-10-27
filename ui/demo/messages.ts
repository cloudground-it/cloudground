// The demo's own words (#39), kept out of ui/i18n/locales so that the real
// panel's bundle does not carry them. Five languages, as everything the
// panel says (tests/demo.test.mjs checks that none is missing).

export const LOCALES = ['it', 'en', 'fr', 'es', 'de'] as const
export type DemoLocale = typeof LOCALES[number]

export interface DemoMessages {
  /** The strip at the foot of every page. */
  banner: string
  /** The link beside it. */
  get: string
  /** What the demo's API answers to a change it will not make. */
  refused: string
  /** What it answers to a page it has no sample data for. */
  missing: string
}

export const MESSAGES: Record<DemoLocale, DemoMessages> = {
  en: {
    banner: 'Demo with sample data: nothing you change here is saved, and nothing leaves your browser.',
    get: 'Get CloudGround',
    refused: 'This is a demo: changes are not saved.',
    missing: 'This page has no sample data in the demo.'
  },
  it: {
    banner: 'Demo con dati di esempio: nulla di ciò che modifichi viene salvato e nulla esce dal tuo browser.',
    get: 'Ottieni CloudGround',
    refused: 'Questa è una demo: le modifiche non vengono salvate.',
    missing: 'Questa pagina non ha dati di esempio nella demo.'
  },
  fr: {
    banner: 'Démo avec des données d\'exemple : rien de ce que vous modifiez n\'est enregistré et rien ne quitte votre navigateur.',
    get: 'Obtenir CloudGround',
    refused: 'Ceci est une démo : les modifications ne sont pas enregistrées.',
    missing: 'Cette page n\'a pas de données d\'exemple dans la démo.'
  },
  es: {
    banner: 'Demo con datos de ejemplo: nada de lo que cambies se guarda y nada sale de tu navegador.',
    get: 'Obtener CloudGround',
    refused: 'Esto es una demo: los cambios no se guardan.',
    missing: 'Esta página no tiene datos de ejemplo en la demo.'
  },
  de: {
    banner: 'Demo mit Beispieldaten: Nichts, was du hier änderst, wird gespeichert, und nichts verlässt deinen Browser.',
    get: 'CloudGround holen',
    refused: 'Dies ist eine Demo: Änderungen werden nicht gespeichert.',
    missing: 'Für diese Seite gibt es in der Demo keine Beispieldaten.'
  }
}

/** The messages for a language (a locale code or an Accept-Language value), English when it is none of the five. */
export function messagesFor(lang: string | null | undefined): DemoMessages {
  const code = String(lang ?? '').slice(0, 2).toLowerCase()
  return (LOCALES as readonly string[]).includes(code) ? MESSAGES[code as DemoLocale] : MESSAGES.en
}
