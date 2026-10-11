// Facts the interface's text states about the product, checked in every
// locale: a hint that names a path or a command must name one that works.
// Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const LOCALES = ['de', 'en', 'es', 'fr', 'it']
const dir = fileURLToPath(new URL('../i18n/locales/', import.meta.url))
const load = loc => JSON.parse(readFileSync(`${dir}${loc}.json`, 'utf8'))
const at = (obj, key) => key.split('.').reduce((o, k) => o?.[k], obj)

// The site's root is root's (the SFTP chroot): an SFTP user cannot make
// deploy/ there. shared/ is the site user's, and a release builds from it.
test('the Releases folder hint names a folder an SFTP user can write', () => {
  for (const loc of LOCALES) {
    const hint = at(load(loc), 'deploy.folderHint')
    assert.match(hint, /shared\/build/, `${loc}: ${hint}`)
    assert.doesNotMatch(hint, /deploy\/build/, `${loc}: ${hint}`)
  }
})
