// The new-site wizard's rules (app/utils/newSite.ts, pages/sites/new.vue):
// what each step stops on, the request Create sends for each type, where a
// server's field error sends the reader back to, and the draft a reload
// keeps, never with a password in it. Node strips the file's types. Run
// with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'

const w = await import('../app/utils/newSite.ts')

const draft = over => ({ ...w.freshDraft(), ...over })
const wp = over => draft({ domain: 'blog.example.com', admin_email: 'owner@example.com', admin_password: 'a-long-admin-pass', db_name: 'blog_example', db_user: 'blog_example', ...over })
const keys = (d, step) => Object.keys(w.stepProblems(d, step)).sort()

test('the five steps, in order', () => {
  assert.deepEqual([...w.WIZARD_STEPS], ['type', 'domain', 'setup', 'database', 'review'])
})

test('a fresh draft is a WordPress site with the recommended profile and a generated database password', () => {
  const d = w.freshDraft()
  assert.equal(d.type, 'wordpress')
  assert.equal(d.profile, 'auto')
  assert.equal(d.admin_user, 'admin')
  assert.equal(w.dbPasswordIssue(d.db_password), undefined)
  assert.notEqual(d.db_password, w.freshDraft().db_password)
})

test('each step stops only on its own fields', () => {
  const d = draft({ type: 'wordpress' })
  assert.deepEqual(keys(d, 'type'), [])
  assert.deepEqual(keys(d, 'domain'), ['domain'])
  assert.deepEqual(keys(d, 'setup'), ['admin_email', 'admin_password'])
  assert.deepEqual(keys(d, 'database'), ['database_name', 'database_user'])
  assert.deepEqual(keys(d, 'review'), ['admin_email', 'admin_password', 'database_name', 'database_user', 'domain'])
  assert.deepEqual(keys(wp(), 'review'), [])
})

test('the setup step checks what the type needs', () => {
  assert.deepEqual(keys(draft({ type: 'proxy' }), 'setup'), ['proxy_upstream'])
  assert.deepEqual(keys(draft({ type: 'proxy', proxy_upstream: 'http://127.0.0.1:3000' }), 'setup'), [])
  assert.deepEqual(keys(draft({ type: 'static' }), 'setup'), [])
  assert.deepEqual(keys(draft({ type: 'laravel' }), 'setup'), [])
  assert.equal(w.stepProblems(wp({ admin_password: 'short' }), 'setup').admin_password.key, 'auth.min12')
  assert.equal(w.stepProblems(wp({ admin_email: 'not-an-email' }), 'setup').admin_email.key, 'newSite.emailRequired')
  assert.equal(w.stepProblems(wp({ admin_user: '  ' }), 'setup').admin_user.key, 'newSite.adminUserRequired')
})

test('the database step: always for WordPress and Laravel, on request for static and PHP, never for a proxy', () => {
  for (const type of ['wordpress', 'woocommerce', 'laravel']) assert.deepEqual(keys(draft({ type }), 'database'), ['database_name', 'database_user'], type)
  for (const type of ['static', 'php']) {
    assert.deepEqual(keys(draft({ type }), 'database'), [], type)
    assert.deepEqual(keys(draft({ type, with_db: true }), 'database'), ['database_name', 'database_user'], type)
  }
  assert.deepEqual(keys(draft({ type: 'proxy', with_db: true }), 'database'), [])
  const p = w.stepProblems(wp({ db_name: 'mysql', db_user: '9bad', db_password: `it's` }), 'database')
  assert.equal(p.database_name.key, 'newSite.nameReserved')
  assert.equal(p.database_user.key, 'dbs.nameRule')
  assert.equal(p.database_password.key, 'newSite.dbPasswordRule')
  assert.equal(w.stepProblems(wp({ db_password: `it's a long one` }), 'database').database_password.key, 'newSite.dbPasswordChars')
})

test('Create sends WordPress with its admin and database, and the profile and PHP only when chosen', () => {
  assert.deepEqual(w.createBody(wp({ domain: ' Blog.Example.com ', title: 'Blog', db_password: 'chosen password 1' })), {
    domain: 'blog.example.com',
    type: 'wordpress',
    title: 'Blog',
    admin_email: 'owner@example.com',
    admin_user: 'admin',
    admin_password: 'a-long-admin-pass',
    database_name: 'blog_example',
    database_user: 'blog_example',
    database_password: 'chosen password 1'
  })
  const chosen = w.createBody(wp({ profile: 'maximum', php_version: '8.4' }))
  assert.equal(chosen.profile, 'maximum')
  assert.equal(chosen.php_version, '8.4')
})

test('Create sends only what the other types use', () => {
  assert.deepEqual(w.createBody(draft({ type: 'proxy', domain: 'app.example.com', proxy_upstream: ' http://127.0.0.1:3000 ', php_version: '8.4', with_db: true })),
    { domain: 'app.example.com', type: 'proxy', proxy_upstream: 'http://127.0.0.1:3000' })
  assert.deepEqual(w.createBody(draft({ type: 'static', domain: 's.example.com', php_version: '8.4' })),
    { domain: 's.example.com', type: 'static', create_database: false })
  const php = w.createBody(draft({ type: 'php', domain: 'p.example.com', with_db: true, db_name: 'p_example', db_user: 'p_example', db_password: 'chosen password 1' }))
  assert.deepEqual(php, { domain: 'p.example.com', type: 'php', create_database: true, database_name: 'p_example', database_user: 'p_example', database_password: 'chosen password 1' })
  const laravel = w.createBody(draft({ type: 'laravel', domain: 'l.example.com', db_name: 'l_example', db_user: 'l_example' }))
  assert.equal(laravel.create_database, undefined)
  assert.equal(laravel.database_name, 'l_example')
  assert.equal(laravel.admin_email, undefined)
})

test('a server error on a field goes back to the earliest step that asks for it', () => {
  assert.equal(w.firstStepOf(['database_name', 'domain']), 'domain')
  assert.equal(w.firstStepOf(['database_user']), 'database')
  assert.equal(w.firstStepOf(['admin_password', 'database_password']), 'setup')
  assert.equal(w.firstStepOf(['type']), 'type')
  assert.equal(w.firstStepOf(['unknown']), undefined)
})

test('the stored draft never holds a password, and reads back field by field', () => {
  const d = wp({ type: 'woocommerce', profile: 'commerce', db_password: 'secret-db-password-1' })
  const raw = w.storeDraft('database', d, { name: true, user: false })
  assert.doesNotMatch(raw, /a-long-admin-pass|secret-db-password-1/)
  assert.doesNotMatch(raw, /admin_password|db_password/)
  const back = w.readDraft(raw)
  assert.equal(back.step, 'database')
  assert.deepEqual(back.edited, { name: true, user: false })
  assert.equal(back.draft.type, 'woocommerce')
  assert.equal(back.draft.domain, 'blog.example.com')
  assert.equal(back.draft.admin_password, undefined)
  assert.equal(back.draft.db_password, undefined)
})

test('a stored draft of the wrong shape is dropped, not trusted', () => {
  assert.equal(w.readDraft(null), null)
  assert.equal(w.readDraft('{not json'), null)
  assert.equal(w.readDraft('42'), null)
  const odd = w.readDraft(JSON.stringify({ step: 'launch', draft: { type: 'exchange', domain: 7, with_db: 'yes', admin_password: 'x'.repeat(20), title: 'T' }, edited: { name: 'yes' } }))
  assert.equal(odd.step, 'type')
  assert.deepEqual(odd.draft, { title: 'T' })
  assert.deepEqual(odd.edited, { name: false, user: false })
})

test('#132: the Domain step checks the aliases and Create sends them cleaned', () => {
  assert.deepEqual(keys(wp({ aliases: ['www.blog.example.com'] }), 'domain'), [])
  assert.equal(w.stepProblems(wp({ aliases: ['not a host'] }), 'domain').aliases.key, 'newSite.wizard.hostInvalid')
  assert.equal(w.stepProblems(wp({ aliases: ['Blog.example.com'] }), 'domain').aliases.key, 'newSite.wizard.aliasTwice')
  assert.equal(w.stepProblems(wp({ aliases: ['a.example.com', 'A.example.com.'] }), 'domain').aliases.key, 'newSite.wizard.aliasTwice')
  assert.equal(w.stepProblems(wp({ aliases: Array.from({ length: 51 }, (_, i) => `a${i}.example.com`) }), 'domain').aliases.key, 'newSite.wizard.aliasesMax')
  assert.equal(w.stepProblems(wp({ domain: 'http://blog.example.com' }), 'domain').domain.key, 'newSite.wizard.hostInvalid')
  assert.equal(w.FIELD_STEP.aliases, 'domain')
  const body = w.createBody(wp({ domain: ' Blog.Example.com. ', aliases: [' WWW.blog.example.com '] }))
  assert.equal(body.domain, 'blog.example.com')
  assert.deepEqual(body.aliases, ['www.blog.example.com'])
  assert.equal('aliases' in w.createBody(wp()), false)
})

test('#132: the DNS check asks for each valid name once, never an address', () => {
  assert.deepEqual(w.checkedNames({ domain: 'Blog.example.com', aliases: ['www.blog.example.com', 'blog.example.com', 'bad name', '192.0.2.1'] }),
    ['blog.example.com', 'www.blog.example.com'])
  assert.deepEqual(w.checkedNames({ domain: '', aliases: [] }), [])
})

test('#132: a reload keeps the aliases, and drops them when they have the wrong shape', () => {
  const kept = w.readDraft(w.storeDraft('domain', wp({ aliases: ['www.blog.example.com'] }), { name: false, user: false }))
  assert.deepEqual(kept.draft.aliases, ['www.blog.example.com'])
  for (const aliases of [null, [1, 2], { a: 1 }]) {
    assert.equal(w.readDraft(JSON.stringify({ step: 'domain', draft: { aliases } })).draft.aliases, undefined, JSON.stringify(aliases))
  }
})
