<script setup lang="ts">
/**
 * A new site, as a wizard on its own page: type and profile, domain, what
 * the type needs, database, review; then the creation's progress and the
 * summary of what its creator needs, the database password included. That
 * password is shown here only (the API answers it once, internal/api SPEC
 * §3.2): the summary can be copied or saved as a .txt, and leaving it
 * unsaved asks first. Each step is checked before the next; the draft
 * survives a reload in session storage, without its passwords. The Domain
 * step takes the aliases too, and says per name whether its DNS already
 * points at this server (GET /api/sites/dns-check, internal/api SPEC
 * §3.14), without ever stopping Next.
 */
import type { SiteType, Task } from '~/types'
import type { FieldProblem, SiteSummary, WizardStep } from '~/utils/newSite'

/** GET /api/sites/dns-check's answer. */
interface DnsAnswer {
  domain: string
  status: 'here' | 'partial' | 'elsewhere' | 'proxied' | 'unresolved' | 'unknown'
  addresses: { family: number, address?: string, private?: boolean, own: boolean }[]
  server: string[]
  cdn?: string
}

definePageMeta({
  middleware: [() => {
    if (!useAuth().isAdmin.value) return navigateTo('/sites', { replace: true })
  }]
})

const { t } = useI18n()
const api = useApi()
const toast = useToast()
const confirm = useConfirm()
const { tasks } = useTasks()
const caps = useCapabilities()
const { sitesVersion } = useShell()
const { copy } = useClipboard({ legacy: true })
useHead({ title: () => t('newSite.wizard.pageTitle') })

const state = reactive(freshDraft())
// The names follow the domain until they are edited.
const edited = reactive({ name: false, user: false })
const show = reactive({ admin: false, db: false })
const step = ref<WizardStep>('type')
const stepIndex = computed(() => WIZARD_STEPS.indexOf(step.value))
// The steps whose Next (or Create) was pressed: their problems show.
const tried = reactive(new Set<WizardStep>())
const serverErrors = ref<Record<string, string>>({})
const busy = ref(false)
const restored = ref(false)
const phase = ref<'form' | 'summary'>('form')

const isWP = computed(() => isWPType(state.type))
const hasPHP = computed(() => hasPHPType(state.type))
const dbIsOptional = computed(() => dbOptional(state.type))
const wantsDatabase = computed(() => wantsDB(state))
const typeName = computed(() => t(`site.type.${state.type}`))

watch(() => state.domain, (d) => {
  const proposed = proposeDBName(d)
  if (!edited.name) state.db_name = proposed
  if (!edited.user) state.db_user = proposed
})
watch(() => [state.domain, state.aliases, state.type, state.with_db, state.db_name, state.db_user, state.db_password], () => { serverErrors.value = {} })

// ---- the choices ----
// The artboard's order: the PHP applications, then the rest.
const typeOrder: SiteType[] = ['wordpress', 'woocommerce', 'php', 'laravel', 'static', 'proxy']
const typeDefs = computed(() => typeOrder.map(v => ({ value: v, label: t(`site.type.${v}`), desc: t(`newSite.typeHint.${v}`), icon: siteTypeIcon[v] })))
const profileDefs = computed(() => [
  { value: 'auto', label: t('site.profileRecommended', { p: t(`site.profile.${autoProfile(state.type)}`) }), desc: t('newSite.wizard.profileAutoHint', { type: typeName.value }) },
  ...(['balanced', 'commerce', 'maximum'] as const).map(v => ({ value: v, label: t(`site.profile.${v}`), desc: t(`site.profileHint.${v}`) }))
])
// The versions installed on the server, as its agent reports them.
const phpItems = computed(() => [
  { label: caps.value?.default_php ? `${t('site.phpDefault')} · PHP ${caps.value.default_php}` : t('site.phpDefault'), value: 'auto' },
  ...(caps.value?.php_versions ?? []).map(v => ({ label: `PHP ${v}`, value: v }))
])
const profileName = computed(() => t(`site.profile.${state.profile === 'auto' ? autoProfile(state.type) : state.profile}`))

// ---- checks (the server's rules; it checks again) ----
const problems = computed(() => draftProblems(state))
const say = (p?: FieldProblem) => p ? t(p.key, p.args ?? {}) : undefined
function shownError(field: string): string | undefined {
  if (serverErrors.value[field]) return serverErrors.value[field]
  const p = problems.value[field]
  if (!p) return undefined
  if (tried.has(FIELD_STEP[field] as WizardStep) || tried.has('review')) return say(p)
  // A database name is checked as it is typed.
  if ((field === 'database_name' && state.db_name) || (field === 'database_user' && state.db_user)) return say(p)
  return undefined
}

// ---- the DNS readiness of each name (never stops Next) ----
// Per name, for the life of the page: an answer, 'checking' or 'failed'
// (a refusal, too many checks, the network). Names are asked one at a
// time, once typing pauses, and only on the Domain step.
const dns = reactive(new Map<string, DnsAnswer | 'checking' | 'failed'>())
const dnsNames = computed(() => checkedNames(state))
const dnsShown = computed(() => dnsNames.value.map(name => ({ name, check: dns.get(name) })))
let dnsQueue: Promise<void> = Promise.resolve()
function askDNS(names: string[], again = false) {
  for (const name of names) {
    const cur = dns.get(name)
    if (cur === 'checking' || (cur && !again)) continue
    dns.set(name, 'checking')
    dnsQueue = dnsQueue.then(async () => {
      try {
        dns.set(name, await api.get<DnsAnswer>(`/api/sites/dns-check?domain=${encodeURIComponent(name)}`))
      } catch {
        dns.set(name, 'failed')
      }
    })
  }
}
const askSoon = useDebounceFn(() => askDNS(dnsNames.value), 700)
watch([dnsNames, step], () => { if (step.value === 'domain' && dnsNames.value.length) void askSoon() }, { immediate: true })
const dnsBusy = computed(() => dnsShown.value.some(x => x.check === 'checking'))

const dnsIcon: Record<string, string> = { here: 'i-cg-check-circle', partial: 'i-cg-alert-triangle', proxied: 'i-cg-alert-triangle', elsewhere: 'i-cg-x-circle', unresolved: 'i-cg-x-circle', unknown: 'i-cg-help', failed: 'i-cg-help', checking: 'i-cg-loader' }
const dnsColor: Record<string, string> = { here: 'text-cg-ok', partial: 'text-cg-warn', proxied: 'text-cg-warn', elsewhere: 'text-cg-err', unresolved: 'text-cg-err', unknown: 'text-muted', failed: 'text-muted', checking: 'text-muted' }
const dnsState = (c?: DnsAnswer | 'checking' | 'failed') => !c ? 'checking' : typeof c === 'string' ? c : c.status
function dnsText(c?: DnsAnswer | 'checking' | 'failed'): string {
  if (!c || c === 'checking') return t('newSite.wizard.dns.checking')
  if (c === 'failed') return t('newSite.wizard.dns.failed')
  const others = c.addresses.filter(a => !a.own).map(a => a.address ?? t('newSite.wizard.dns.private'))
  return t(`newSite.wizard.dns.${c.status}`, { addrs: [...new Set(others)].join(', '), server: c.server.join(', ') })
}

// ---- moving between steps ----
const formEl = ref<HTMLFormElement | null>(null)
const headingEl = ref<HTMLElement | null>(null)
const announce = ref('')

async function goTo(s: WizardStep, focus: 'heading' | 'invalid' = 'heading') {
  step.value = s
  announce.value = t('newSite.wizard.stepOf', { n: WIZARD_STEPS.indexOf(s) + 1, total: WIZARD_STEPS.length, name: t(`newSite.wizard.steps.${s}`) })
  await nextTick()
  if (focus === 'invalid') {
    const bad = formEl.value?.querySelector<HTMLElement>('[aria-invalid=true]')
    if (bad) return bad.focus()
  }
  headingEl.value?.focus()
}
async function next() {
  tried.add(step.value)
  if (Object.keys(stepProblems(state, step.value)).length) {
    await nextTick()
    formEl.value?.querySelector<HTMLElement>('[aria-invalid=true]')?.focus()
    return
  }
  if (step.value === 'review') return create()
  await goTo(WIZARD_STEPS[stepIndex.value + 1]!)
}
function back() {
  if (stepIndex.value > 0) void goTo(WIZARD_STEPS[stepIndex.value - 1]!)
}

// ---- the draft across a reload ----
onMounted(() => {
  let raw: string | null = null
  try {
    raw = sessionStorage.getItem(DRAFT_KEY)
  } catch {
    // Storage blocked: the wizard starts empty.
  }
  const d = readDraft(raw)
  if (!d) return
  Object.assign(edited, d.edited)
  Object.assign(state, d.draft)
  // Back to the first step that needs something again (a password is
  // never stored), no further than where the reload happened.
  const upTo = WIZARD_STEPS.indexOf(d.step)
  const missing = WIZARD_STEPS.slice(0, upTo).find(s => Object.keys(stepProblems(state, s)).length)
  step.value = missing ?? d.step
  restored.value = upTo > 0 && (isWP.value || wantsDatabase.value)
})
function saveDraft() {
  try {
    sessionStorage.setItem(DRAFT_KEY, storeDraft(step.value, state, edited))
  } catch {
    // Storage blocked or full: the draft lives only in this page.
  }
}
function dropDraft() {
  try {
    sessionStorage.removeItem(DRAFT_KEY)
  } catch {
    // Nothing stored.
  }
}
watch([state, step, edited], () => { if (phase.value === 'form') saveDraft() }, { deep: true })

// ---- creating ----
const created = ref<{ id: number, domain: string, task: Task, summary: SiteSummary } | null>(null)
const saved = ref(false)
const copiedAll = ref(false)

async function create() {
  tried.add('review')
  if (Object.keys(problems.value).length) return goTo(firstStepOf(Object.keys(problems.value)) ?? 'type', 'invalid')
  if (busy.value) return
  busy.value = true
  try {
    const res = await api.post<{ site: { id: number, domain: string }, task: Task, summary: SiteSummary }>('/api/sites', createBody(state))
    created.value = { id: res.site.id, domain: res.site.domain, task: res.task, summary: res.summary }
    saved.value = !res.summary.database
    phase.value = 'summary'
    dropDraft()
    sitesVersion.value++
    await nextTick()
    headingEl.value?.focus()
  } catch (e) {
    const fields = isApiError(e) && e.data && typeof e.data.fields === 'object' ? e.data.fields as Record<string, string> : null
    if (fields && Object.keys(fields).length) {
      // A name in use (409) is said in the reader's language; other field
      // problems are the server's words.
      const taken = isApiError(e) && e.status === 409
      serverErrors.value = Object.fromEntries(Object.entries(fields).map(([k, v]) => [k,
        taken && k === 'database_name' ? t('newSite.nameTaken', { name: state.db_name })
          : taken && k === 'database_user' ? t('newSite.nameTaken', { name: state.db_user }) : v]))
      const at = firstStepOf(Object.keys(fields))
      if (at) await goTo(at, 'invalid')
    }
    toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
  } finally {
    busy.value = false
  }
}

function cancel() {
  dropDraft()
  void navigateTo('/sites')
}

// What the site will get, beside every step (the artboard's "will create").
const will = computed(() => {
  const out = [t('newSite.willUser')]
  if (state.type !== 'proxy' && wantsDatabase.value) out.push(state.db_name ? t('newSite.willDatabase', { name: state.db_name }) : t('newSite.wizard.willDatabaseAny'))
  out.push(t('newSite.willCert'))
  out.push(t('newSite.willProfile', { profile: profileName.value }))
  if (state.type !== 'proxy') out.push(t('newSite.willBackups'))
  return out
})

// The review: one part per step, with a way back to it.
interface ReviewRow { label: string, value: string, mono?: boolean }
const review = computed<{ step: WizardStep, rows: ReviewRow[] }[]>(() => {
  const pw = (s: string) => t('newSite.wizard.pwSet', { n: s.length })
  const setup: ReviewRow[] = []
  if (hasPHP.value) setup.push({ label: 'PHP', value: phpItems.value.find(i => i.value === state.php_version)?.label ?? state.php_version })
  if (isWP.value) {
    setup.push({ label: t('site.title'), value: state.title || t('newSite.wizard.noTitle') },
      { label: t('site.adminEmail'), value: state.admin_email.trim(), mono: true },
      { label: t('site.adminUser'), value: state.admin_user.trim(), mono: true },
      { label: t('site.adminPassword'), value: pw(state.admin_password) })
  }
  if (state.type === 'proxy') setup.push({ label: t('site.upstream'), value: state.proxy_upstream.trim(), mono: true })
  const db: ReviewRow[] = wantsDatabase.value
    ? [{ label: t('newSite.dbName'), value: state.db_name, mono: true }, { label: t('newSite.dbUser'), value: state.db_user, mono: true }, { label: t('newSite.dbPassword'), value: pw(state.db_password) }]
    : [{ label: t('newSite.dbPart'), value: t('newSite.wizard.none') }]
  return [
    { step: 'type', rows: [{ label: t('site.typeLabel'), value: typeName.value }, { label: t('site.profileLabel'), value: profileName.value }] },
    { step: 'domain', rows: [{ label: t('site.domain'), value: cleanHost(state.domain), mono: true },
      ...(state.aliases.length ? [{ label: t('address.aliases'), value: state.aliases.map(cleanHost).join(', '), mono: true }] : [])] },
    ...(setup.length ? [{ step: 'setup' as const, rows: setup }] : []),
    { step: 'database', rows: db }
  ]
})

// ---- the summary ----
const task = computed<Task | null>(() => {
  const c = created.value
  if (!c) return null
  return tasks.value.find(x => x.id === c.task.id) ?? c.task
})
const seen = ref<string[]>([])
watch(() => task.value?.message, (m) => {
  if (m && !seen.value.includes(m)) seen.value = [...seen.value, m]
}, { immediate: true })
const taskState = computed(() => task.value?.status ?? 'pending')
const progress = computed(() => seen.value.map((what, i) => {
  const last = i === seen.value.length - 1
  const st = !last ? 'done' : taskState.value === 'succeeded' ? 'done' : taskState.value === 'failed' ? 'failed' : 'running'
  return { n: String(i + 1).padStart(2, '0'), what, st }
}))
const stateColor = (st: string) => st === 'done' ? 'text-cg-ok' : st === 'failed' ? 'text-cg-err' : 'text-cg-blue'

const origin = import.meta.client ? window.location.origin : ''

// The summary's rows: a part's heading, or a label and its value.
interface Row { part?: string, label?: string, value?: string, href?: string, panel?: boolean, copy?: boolean }
const rows = computed<Row[]>(() => {
  const s = created.value?.summary
  if (!s) return []
  const out: Row[] = [
    { part: t('newSite.partSite') },
    { label: t('newSite.siteUrl'), value: s.site_url, href: s.site_url },
    { label: t('newSite.panelUrl'), value: panelLink(s, origin), panel: true }
  ]
  if (s.wordpress) {
    out.push({ part: 'WordPress' },
      { label: t('newSite.adminUrl'), value: s.wordpress.admin_url, href: s.wordpress.admin_url },
      { label: t('newSite.adminUser'), value: s.wordpress.admin_user, copy: true })
  }
  const d = s.database
  if (d) {
    out.push({ part: t('newSite.partDatabase') },
      { label: t('newSite.dbHost'), value: d.host, copy: true },
      { label: t('newSite.dbPort'), value: String(d.port) })
    if (d.socket) out.push({ label: t('newSite.dbSocket'), value: d.socket, copy: true })
    out.push({ label: t('newSite.dbName'), value: d.name, copy: true },
      { label: t('newSite.dbUser'), value: d.user, copy: true },
      { label: t('newSite.dbPassword'), value: d.password, copy: true })
  }
  out.push({ part: 'SFTP' })
  if (s.sftp.enabled) {
    out.push({ label: t('newSite.sftpHost'), value: s.sftp.host ?? '', copy: true },
      { label: t('newSite.sftpPort'), value: String(s.sftp.port ?? 22) },
      { label: t('newSite.sftpUser'), value: s.sftp.user, copy: true })
  } else {
    out.push({ label: t('newSite.sftpUser'), value: t('newSite.sftpOff', { user: s.sftp.user }) })
  }
  return out
})
const text = computed(() => created.value ? summaryText(created.value.summary, created.value.domain, origin, t) : '')
function copyAll() {
  copy(text.value)
  saved.value = true
  copiedAll.value = true
  setTimeout(() => { copiedAll.value = false }, 2000)
}
function download() {
  if (!created.value) return
  downloadText(`${created.value.domain}-cloudground.txt`, text.value)
  saved.value = true
}

// Leaving the summary unsaved loses the password: a reload or another
// page asks first, in the browser's words or ours.
const unsaved = computed(() => phase.value === 'summary' && !saved.value)
useEventListener(window, 'beforeunload', (ev: BeforeUnloadEvent) => {
  if (unsaved.value) {
    ev.preventDefault()
    ev.returnValue = ''
  }
})
onBeforeRouteLeave(async () => {
  if (!unsaved.value) return true
  return await confirm({ title: t('newSite.closeTitle'), description: t('newSite.closeHint'), confirmLabel: t('newSite.closeAnyway'), danger: true })
})
</script>

<template>
  <UDashboardPanel id="new-site">
    <template #header>
      <PageNavbar :title="t('site.new')" />
    </template>

    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader
          :title="phase === 'form' ? t('site.new') : created?.domain"
          :description="phase === 'form' ? t('site.newHint') : t('newSite.summaryHint')"
        />

        <!-- The wizard -->
        <form v-if="phase === 'form'" ref="formEl" class="flex flex-col gap-7" novalidate data-testid="site-wizard" @submit.prevent="next">
          <nav :aria-label="t('newSite.wizard.stepsLabel')">
            <ol class="m-0 flex list-none border border-default p-0">
              <li
                v-for="(s, i) in WIZARD_STEPS"
                :key="s"
                :aria-current="s === step ? 'step' : undefined"
                class="flex min-w-0 items-center border-default not-first:border-s"
                :class="s === step ? 'flex-1 bg-muted font-semibold text-highlighted shadow-[inset_0_-2px_0_var(--cg-ink)]' : 'flex-none text-muted sm:flex-1'"
              >
                <component
                  :is="i < stepIndex ? 'button' : 'span'"
                  :type="i < stepIndex ? 'button' : undefined"
                  class="flex w-full min-w-0 items-center gap-2.5 px-2.5 py-2 text-[13px] sm:px-4 sm:py-2.5 sm:text-sm"
                  :class="i < stepIndex ? 'cursor-pointer text-start hover:bg-muted hover:text-highlighted' : ''"
                  @click="i < stepIndex && goTo(s)"
                >
                  <span
                    class="mono inline-grid size-[22px] shrink-0 place-items-center border text-xs font-semibold"
                    :class="s === step ? 'border-cg-ink bg-cg-ink text-cg-paper' : i < stepIndex ? 'border-cg-blue text-cg-blue' : 'border-cg-rule-strong'"
                    aria-hidden="true"
                  >
                    <UIcon v-if="i < stepIndex" name="i-cg-check" class="size-3.5" />
                    <template v-else>{{ i + 1 }}</template>
                  </span>
                  <span class="sr-only">{{ t('newSite.wizard.stepOf', { n: i + 1, total: WIZARD_STEPS.length, name: t(`newSite.wizard.steps.${s}`) }) }}</span>
                  <span class="min-w-0 truncate" :class="s === step ? '' : 'max-sm:hidden'" aria-hidden="true">{{ t(`newSite.wizard.steps.${s}`) }}</span>
                </component>
              </li>
            </ol>
          </nav>
          <p class="sr-only" aria-live="polite">{{ announce }}</p>

          <UAlert v-if="restored" color="info" variant="subtle" icon="i-cg-info" :title="t('newSite.wizard.draftRestored')" :close="{ 'aria-label': t('newSite.close') }" @update:open="restored = false" />

          <div class="grid items-start gap-8 lg:grid-cols-[minmax(0,1fr)_300px]">
            <section class="flex min-w-0 flex-col gap-7" :aria-labelledby="`ws-${step}`">
              <!-- 1. Type and profile -->
              <template v-if="step === 'type'">
                <div class="flex flex-col">
                  <h2 :id="`ws-${step}`" ref="headingEl" tabindex="-1" class="m-0 text-[1.0625rem] text-highlighted outline-none">{{ t('newSite.wizard.typeLegend') }}</h2>
                  <p class="mt-1 mb-3.5 text-sm text-muted">{{ t('newSite.wizard.typeHint') }}</p>
                  <div role="radiogroup" :aria-labelledby="`ws-${step}`" class="grid grid-cols-1 border-s border-t border-cg-rule-strong sm:grid-cols-2 xl:grid-cols-3">
                    <label
                      v-for="d in typeDefs"
                      :key="d.value"
                      class="relative grid cursor-pointer grid-cols-[44px_minmax(0,1fr)_20px] items-center gap-3.5 border-e border-b border-cg-rule-strong bg-default p-4 text-highlighted transition-colors duration-100 hover:bg-muted has-checked:bg-cg-blue-tint has-checked:shadow-[inset_0_0_0_2px_var(--cg-blue)] has-focus-visible:z-[1] has-focus-visible:outline-2 has-focus-visible:outline-offset-2 has-focus-visible:outline-cg-blue"
                    >
                      <input v-model="state.type" type="radio" name="site-type" :value="d.value" class="absolute inset-0 m-0 cursor-pointer opacity-0" :data-type="d.value">
                      <UIcon
                        :name="d.icon"
                        class="justify-self-center"
                        :class="[d.value === 'woocommerce' ? 'h-[27px] w-[44px]' : 'size-7', state.type === d.value ? 'text-cg-blue' : 'text-highlighted']"
                        aria-hidden="true"
                      />
                      <span class="min-w-0">
                        <span class="block font-semibold">{{ d.label }}</span>
                        <span class="block text-[13px] text-muted">{{ d.desc }}</span>
                      </span>
                      <span
                        class="grid size-5 place-items-center rounded-full border"
                        :class="state.type === d.value ? 'border-cg-blue bg-cg-blue' : 'border-cg-rule-strong'"
                        aria-hidden="true"
                      >
                        <span v-if="state.type === d.value" class="size-2 rounded-full bg-cg-paper" />
                      </span>
                    </label>
                  </div>
                </div>

                <fieldset class="m-0 min-w-0 border-0 p-0">
                  <legend class="p-0 text-[1.0625rem] font-semibold text-highlighted">{{ t('newSite.wizard.profileLegend') }}</legend>
                  <p class="mt-1 mb-3.5 text-sm text-muted">{{ t('newSite.wizard.profileHint') }}</p>
                  <div class="grid border border-cg-rule-strong">
                    <label
                      v-for="p in profileDefs"
                      :key="p.value"
                      class="grid cursor-pointer grid-cols-[20px_minmax(0,1fr)] items-start gap-3 border-b border-default px-4 py-3 transition-colors duration-100 last:border-b-0 hover:bg-muted has-checked:bg-cg-blue-tint"
                    >
                      <input v-model="state.profile" type="radio" name="site-profile" :value="p.value" class="mt-[3px] size-[18px] accent-(--cg-blue)">
                      <span class="min-w-0">
                        <span class="block font-semibold text-highlighted">{{ p.label }}</span>
                        <span class="block text-sm text-muted">{{ p.desc }}</span>
                      </span>
                    </label>
                  </div>
                </fieldset>
              </template>

              <!-- 2. Domain -->
              <template v-else-if="step === 'domain'">
                <div>
                  <h2 :id="`ws-${step}`" ref="headingEl" tabindex="-1" class="m-0 text-[1.0625rem] text-highlighted outline-none">{{ t('newSite.wizard.domainLegend') }}</h2>
                  <p class="mt-1 mb-0 max-w-[72ch] text-sm text-muted">{{ t('newSite.wizard.domainHint') }}</p>
                </div>
                <UFormField :label="t('site.domain')" required :error="shownError('domain')" class="max-w-[36rem]">
                  <UInput v-model="state.domain" placeholder="example.com" class="w-full" size="lg" :ui="{ base: 'font-mono' }" autocomplete="off" autocapitalize="off" spellcheck="false" inputmode="url" />
                </UFormField>
                <UFormField :label="t('address.aliases')" :help="t('newSite.wizard.aliasesHint')" :error="shownError('aliases')" class="max-w-[36rem]">
                  <UInputTags
                    v-model="state.aliases"
                    :placeholder="`www.${cleanHost(state.domain) || 'example.com'}`"
                    :max="MAX_ALIASES"
                    class="w-full font-mono"
                    autocapitalize="off"
                    spellcheck="false"
                    data-testid="site-aliases"
                  />
                </UFormField>

                <section v-if="dnsShown.length" class="flex max-w-[48rem] flex-col border border-default" aria-labelledby="ws-dns" data-testid="site-dns">
                  <div class="flex items-center justify-between gap-3 border-b border-default bg-muted px-4 py-2">
                    <h3 id="ws-dns" class="m-0 text-[13px] font-semibold text-highlighted">{{ t('newSite.wizard.dns.title') }}</h3>
                    <UButton
                      color="secondary"
                      variant="link"
                      size="sm"
                      class="px-0"
                      icon="i-cg-refresh"
                      :label="t('newSite.wizard.dns.again')"
                      :disabled="dnsBusy"
                      @click="askDNS(dnsNames, true)"
                    />
                  </div>
                  <ul class="m-0 list-none p-0" aria-live="polite">
                    <li v-for="x in dnsShown" :key="x.name" class="grid grid-cols-[18px_minmax(0,1fr)] items-start gap-3 border-b border-default px-4 py-2.5 last:border-b-0" :data-dns="dnsState(x.check)">
                      <UIcon
                        :name="dnsIcon[dnsState(x.check)]!"
                        class="mt-0.5 size-[18px]"
                        :class="[dnsColor[dnsState(x.check)], dnsState(x.check) === 'checking' ? 'animate-spin motion-reduce:animate-none' : '']"
                        aria-hidden="true"
                      />
                      <span class="min-w-0">
                        <span class="mono block text-[13px] text-highlighted [overflow-wrap:anywhere]">{{ x.name }}</span>
                        <span class="block text-[13px] text-muted">{{ dnsText(x.check) }}</span>
                      </span>
                    </li>
                  </ul>
                  <p class="m-0 border-t border-default px-4 py-2 text-[13px] text-muted">{{ t('newSite.wizard.dns.hint') }}</p>
                </section>
              </template>

              <!-- 3. What the type needs -->
              <template v-else-if="step === 'setup'">
                <div>
                  <h2 :id="`ws-${step}`" ref="headingEl" tabindex="-1" class="m-0 text-[1.0625rem] text-highlighted outline-none">{{ t('newSite.wizard.setupLegend') }}</h2>
                  <p class="mt-1 mb-0 text-sm text-muted">{{ state.type === 'static' ? t('newSite.wizard.setupNone') : t('newSite.wizard.setupHint', { type: typeName }) }}</p>
                </div>

                <UFormField v-if="state.type === 'proxy'" :label="t('site.upstream')" required :error="shownError('proxy_upstream')" :help="t('newSite.upstreamHelp')">
                  <UInput v-model="state.proxy_upstream" placeholder="http://127.0.0.1:3000" class="w-full" :ui="{ base: 'font-mono' }" autocomplete="off" spellcheck="false" />
                </UFormField>

                <UFormField v-if="hasPHP" label="PHP" :help="t('newSite.wizard.phpHint')" class="max-w-[24rem]">
                  <USelect v-model="state.php_version" :items="phpItems" class="w-full" />
                </UFormField>

                <section v-if="isWP" class="flex flex-col gap-4 border-t border-default pt-5" aria-labelledby="ws-wp">
                  <h3 id="ws-wp" class="m-0 text-[15px] text-highlighted">{{ t('newSite.wpPart') }}</h3>
                  <UFormField :label="t('site.title')">
                    <UInput v-model="state.title" class="w-full" />
                  </UFormField>
                  <div class="grid gap-4 sm:grid-cols-2">
                    <UFormField :label="t('site.adminEmail')" required :error="shownError('admin_email')">
                      <UInput v-model="state.admin_email" type="email" class="w-full" autocomplete="email" />
                    </UFormField>
                    <UFormField :label="t('site.adminUser')" required :error="shownError('admin_user')">
                      <UInput v-model="state.admin_user" class="w-full" autocomplete="off" />
                    </UFormField>
                  </div>
                  <UFormField :label="t('site.adminPassword')" :hint="t('auth.min12')" required :error="shownError('admin_password')">
                    <div class="flex gap-2">
                      <UInput v-model="state.admin_password" :type="show.admin ? 'text' : 'password'" autocomplete="new-password" class="min-w-0 flex-1" :ui="{ base: 'font-mono', trailing: 'pe-1' }">
                        <template #trailing>
                          <UButton color="neutral" variant="link" size="sm" square :icon="show.admin ? 'i-cg-eye-off' : 'i-cg-eye'" :aria-label="show.admin ? t('newSite.hide') : t('common.show')" @click="show.admin = !show.admin" />
                        </template>
                      </UInput>
                      <UButton color="neutral" variant="outline" :label="t('newSite.generate')" @click="state.admin_password = generatePassword(); show.admin = true" />
                    </div>
                  </UFormField>
                </section>
              </template>

              <!-- 4. Database -->
              <template v-else-if="step === 'database'">
                <div>
                  <h2 :id="`ws-${step}`" ref="headingEl" tabindex="-1" class="m-0 text-[1.0625rem] text-highlighted outline-none">{{ t('newSite.wizard.dbLegend') }}</h2>
                  <p class="mt-1 mb-0 text-sm text-muted">
                    {{ state.type === 'proxy' ? t('newSite.wizard.dbNone') : dbIsOptional ? t('newSite.withDatabaseHint') : t('newSite.dbAlways', { type: typeName }) }}
                  </p>
                </div>
                <USwitch v-if="dbIsOptional" v-model="state.with_db" :label="t('newSite.withDatabase')" />
                <template v-if="wantsDatabase">
                  <div class="flex flex-col gap-2">
                    <div class="grid gap-4 sm:grid-cols-2">
                      <UFormField :label="t('newSite.dbName')" required :error="shownError('database_name')">
                        <UInput v-model="state.db_name" class="w-full" :ui="{ base: 'font-mono' }" autocomplete="off" spellcheck="false" @input="edited.name = true" />
                      </UFormField>
                      <UFormField :label="t('newSite.dbUser')" required :error="shownError('database_user')">
                        <UInput v-model="state.db_user" class="w-full" :ui="{ base: 'font-mono' }" autocomplete="off" spellcheck="false" @input="edited.user = true" />
                      </UFormField>
                    </div>
                    <p class="m-0 text-[13px] leading-normal text-muted">{{ t('dbs.nameHint') }}</p>
                  </div>
                  <UFormField :label="t('newSite.dbPassword')" :hint="t('auth.min12')" required :error="shownError('database_password')" :help="t('newSite.dbPasswordHelp')">
                    <div class="flex gap-2">
                      <UInput v-model="state.db_password" :type="show.db ? 'text' : 'password'" autocomplete="new-password" class="min-w-0 flex-1" :ui="{ base: 'font-mono', trailing: 'pe-1' }">
                        <template #trailing>
                          <UButton color="neutral" variant="link" size="sm" square :icon="show.db ? 'i-cg-eye-off' : 'i-cg-eye'" :aria-label="show.db ? t('newSite.hide') : t('common.show')" @click="show.db = !show.db" />
                        </template>
                      </UInput>
                      <UButton color="neutral" variant="outline" :label="t('newSite.generate')" @click="state.db_password = generatePassword(); show.db = true" />
                    </div>
                  </UFormField>
                </template>
              </template>

              <!-- 5. Review -->
              <template v-else>
                <div>
                  <h2 :id="`ws-${step}`" ref="headingEl" tabindex="-1" class="m-0 text-[1.0625rem] text-highlighted outline-none">{{ t('newSite.wizard.reviewLegend') }}</h2>
                  <p class="mt-1 mb-0 text-sm text-muted">{{ t('newSite.wizard.reviewHint') }}</p>
                </div>
                <dl class="m-0 border border-default" data-testid="site-review">
                  <template v-for="part in review" :key="part.step">
                    <dt class="flex items-center justify-between gap-3 border-b border-default bg-muted px-4 py-1.5 text-[13px] font-semibold text-highlighted">
                      {{ t(`newSite.wizard.steps.${part.step}`) }}
                      <UButton
                        color="secondary"
                        variant="link"
                        size="sm"
                        class="px-0"
                        :label="t('newSite.wizard.change')"
                        :aria-label="t('newSite.wizard.changeStep', { step: t(`newSite.wizard.steps.${part.step}`) })"
                        @click="goTo(part.step)"
                      />
                    </dt>
                    <dd class="m-0">
                      <dl class="m-0 grid grid-cols-1 sm:grid-cols-[minmax(0,200px)_minmax(0,1fr)]">
                        <template v-for="r in part.rows" :key="r.label">
                          <dt class="px-4 pt-2.5 text-[13px] text-muted sm:border-b sm:border-default sm:py-2.5">{{ r.label }}</dt>
                          <dd class="m-0 border-b border-default px-4 pb-2.5 text-sm text-highlighted [overflow-wrap:anywhere] sm:py-2.5" :class="r.mono ? 'mono text-[13px]' : ''">{{ r.value }}</dd>
                        </template>
                      </dl>
                    </dd>
                  </template>
                </dl>
              </template>
            </section>

            <aside class="border border-default" :aria-labelledby="'ws-will'">
              <h2 id="ws-will" class="m-0 border-b border-default bg-muted px-4 py-3 text-[0.9375rem] text-highlighted">{{ t('newSite.wizard.willHeading', { type: typeName }) }}</h2>
              <ul class="m-0 list-none p-0">
                <li v-for="w in will" :key="w" class="flex items-start gap-2.5 border-b border-default px-4 py-2.5 text-[0.9375rem] last:border-b-0 [overflow-wrap:anywhere]">
                  <UIcon name="i-cg-check" class="mt-0.5 size-[18px] shrink-0 text-cg-ok" aria-hidden="true" />
                  <span class="min-w-0">{{ w }}</span>
                </li>
              </ul>
            </aside>
          </div>

          <div class="flex flex-wrap justify-between gap-3 border-t border-default pt-5">
            <UButton color="neutral" variant="ghost" :label="t('common.cancel')" class="max-sm:order-2" @click="cancel" />
            <div class="flex gap-2 max-sm:w-full">
              <UButton color="neutral" variant="outline" icon="i-cg-arrow-left" :label="t('common.back')" :disabled="stepIndex === 0" class="max-sm:flex-1 max-sm:justify-center" @click="back" />
              <UButton
                type="submit"
                color="primary"
                variant="solid"
                :label="step === 'review' ? t('site.create') : t('common.continue')"
                :trailing-icon="step === 'review' ? undefined : 'i-cg-arrow-right'"
                :loading="busy"
                class="max-sm:flex-1 max-sm:justify-center"
                data-testid="wizard-next"
              />
            </div>
          </div>
        </form>

        <!-- The summary -->
        <div v-else-if="created" class="grid items-start gap-8 lg:grid-cols-[minmax(0,1fr)_300px]" data-testid="site-summary">
          <div class="flex min-w-0 flex-col gap-5">
            <h2 ref="headingEl" tabindex="-1" class="sr-only">{{ created.domain }}</h2>
            <dl class="m-0 grid grid-cols-1 border border-default sm:grid-cols-[minmax(0,200px)_minmax(0,1fr)]">
              <template v-for="(row, i) in rows" :key="i">
                <dt v-if="row.part" class="border-b border-default bg-muted px-4 py-2 text-[13px] font-semibold text-highlighted sm:col-span-2">{{ row.part }}</dt>
                <template v-else>
                  <dt class="px-4 pt-2.5 text-[13px] text-muted sm:border-b sm:border-default sm:py-2.5">{{ row.label }}</dt>
                  <dd class="m-0 flex min-w-0 items-center gap-2 border-b border-default px-4 pb-2.5 sm:py-2">
                    <a v-if="row.href" :href="row.href" target="_blank" rel="noopener" class="mono link min-w-0 flex-1 text-[13px] [overflow-wrap:anywhere]">{{ row.value }}</a>
                    <NuxtLink v-else-if="row.panel" :to="`/sites/${created.id}`" class="mono link min-w-0 flex-1 text-[13px] [overflow-wrap:anywhere]">{{ row.value }}</NuxtLink>
                    <span v-else class="mono min-w-0 flex-1 text-[13px] text-highlighted [overflow-wrap:anywhere]">{{ row.value }}</span>
                    <CopyButton v-if="row.copy" :value="row.value ?? ''" />
                  </dd>
                </template>
              </template>
            </dl>
            <UAlert v-if="created.summary.database" color="warning" variant="subtle" icon="i-cg-key" :title="t('newSite.onceTitle')" :description="t('newSite.onceHint')" />
            <div class="flex flex-wrap items-center gap-2.5 border-t border-default pt-5">
              <UButton color="neutral" variant="outline" icon="i-cg-download" :label="t('newSite.download')" data-testid="summary-download" @click="download" />
              <UButton color="neutral" variant="outline" :icon="copiedAll ? 'i-cg-check' : 'i-cg-copy'" :label="copiedAll ? t('common.copied') : t('common.copy')" data-testid="summary-copy" @click="copyAll" />
              <span class="hidden flex-1 sm:block" />
              <UButton color="neutral" variant="ghost" :label="t('newSite.close')" to="/sites" data-testid="summary-close" />
              <UButton color="primary" variant="solid" :label="t('newSite.openSite')" trailing-icon="i-cg-arrow-right" :to="`/sites/${created.id}`" />
            </div>
          </div>

          <!-- First on a phone: what is happening now comes before the details. -->
          <section class="border border-default max-lg:order-first" aria-labelledby="ws-progress">
            <h2 id="ws-progress" class="m-0 border-b border-default bg-muted px-4 py-3 text-[0.9375rem] text-highlighted">{{ t('newSite.progress') }}</h2>
            <ol class="m-0 list-none p-0" aria-live="polite">
              <li v-for="s in progress" :key="s.n" class="flex items-start gap-3 border-b border-default px-4 py-2.5 last:border-b-0">
                <span class="mono min-w-[22px] text-[13px] leading-6 text-cg-blue">{{ s.n }}</span>
                <span class="min-w-0 flex-1 text-sm leading-6 text-highlighted [overflow-wrap:anywhere]">{{ s.what }}</span>
                <span class="shrink-0 text-end text-[13px] leading-6 font-medium" :class="stateColor(s.st)">{{ t(`newSite.step.${s.st}`) }}</span>
              </li>
              <li v-if="!progress.length" class="flex items-start gap-3 px-4 py-2.5">
                <span class="mono min-w-[22px] text-[13px] leading-6 text-cg-blue">01</span>
                <span class="min-w-0 flex-1 text-sm leading-6 text-highlighted">{{ t('newSite.queued') }}</span>
                <span class="shrink-0 text-[13px] leading-6 font-medium text-cg-blue">{{ t('newSite.step.running') }}</span>
              </li>
            </ol>
            <UAlert v-if="taskState === 'failed'" color="error" variant="subtle" icon="i-cg-x-circle" class="rounded-none border-0 border-t" :title="t('newSite.failed')" :description="task?.error" />
          </section>
        </div>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
