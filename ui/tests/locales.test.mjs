// Every locale must carry exactly en.json's keys, with the same placeholders
// and plural forms, in messages vue-i18n can compile. Three sets are
// checked: i18n/locales/ (the interface), i18n/design/ (the panel design's
// `cg.` strings) and i18n/ux/ (the layout system's `ux.` strings), the last
// two merged in by i18n/i18n.config.ts. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { baseCompile } from '@intlify/message-compiler'

const LOCALES = ['de.json', 'en.json', 'es.json', 'fr.json', 'it.json']

/** Flattens to { 'a.b.c': message }, and reports keys that are not plain. */
function flatten(obj, prefix, out, problems) {
  for (const [key, value] of Object.entries(obj)) {
    const path = prefix ? `${prefix}.${key}` : key
    if (key.includes('.')) problems.push(`key ${JSON.stringify(path)} contains a dot`)
    if (key === '') problems.push(`empty key under ${JSON.stringify(prefix)}`)
    if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
      flatten(value, path, out, problems)
    } else if (typeof value === 'string') {
      out[path] = value
    } else {
      problems.push(`${path} is not a string`)
    }
  }
  return out
}

const placeholders = msg => [...msg.matchAll(/\{\s*([^{}]*?)\s*\}/g)].map(m => m[1]).sort()
const pluralForms = msg => msg.split(/(?<!\\)\|/).length

function loadSet(name) {
  const dir = fileURLToPath(new URL(`../i18n/${name}/`, import.meta.url))
  const load = file => JSON.parse(readFileSync(dir + file, 'utf8'))
  const files = readdirSync(dir).filter(f => f.endsWith('.json')).sort()
  return { files, load }
}

function checkSet(name) {
  const { files, load } = loadSet(name)
  const refProblems = []
  const ref = flatten(load('en.json'), '', {}, refProblems)

  test(`${name}: the five interface languages are all here`, () => {
    assert.deepEqual(files, LOCALES)
  })

  for (const file of files) {
    test(`${name}/${file}`, () => {
      const problems = file === 'en.json' ? [...refProblems] : []
      const msgs = file === 'en.json' ? ref : flatten(load(file), '', {}, problems)

      for (const key of Object.keys(ref)) {
        if (!(key in msgs)) problems.push(`missing ${key}`)
      }
      for (const [key, msg] of Object.entries(msgs)) {
        if (!(key in ref)) {
          problems.push(`extra ${key}`)
          continue
        }
        const want = placeholders(ref[key])
        const got = placeholders(msg)
        if (JSON.stringify(got) !== JSON.stringify(want)) {
          problems.push(`${key}: placeholders {${got.join('}, {')}} instead of {${want.join('}, {')}}`)
        }
        if (pluralForms(msg) !== pluralForms(ref[key])) {
          problems.push(`${key}: ${pluralForms(msg)} plural forms instead of ${pluralForms(ref[key])}`)
        }
        if (msg.trim() === '') problems.push(`${key} is empty`)
        baseCompile(msg, {
          onError: (e) => { problems.push(`${key}: vue-i18n cannot compile it (${e.message})`) }
        })
      }
      assert.deepEqual(problems, [], `${name}/${file}:\n  ${problems.join('\n  ')}`)
    })
  }
}

checkSet('locales')
checkSet('design')
checkSet('ux')

// The three sets are merged into one message tree per locale: a top-level key
// present in two would have one silently override the other.
test('design, ux and locales do not share a top-level key', () => {
  const sets = ['locales', 'design', 'ux'].map(n => Object.keys(loadSet(n).load('en.json')))
  const seen = new Set()
  const shared = []
  for (const keys of sets) {
    for (const k of keys) {
      if (seen.has(k)) shared.push(k)
      seen.add(k)
    }
  }
  assert.deepEqual(shared, [])
})
