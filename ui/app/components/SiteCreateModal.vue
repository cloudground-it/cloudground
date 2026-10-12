<script setup lang="ts">
/**
 * The new-site dialog (artboard PanelNewSite, with a database part): the
 * form, then the summary of the site just created with what its creator
 * needs to use it, the database password included. That password is
 * shown here only (the API answers it once, internal/api SPEC §3.2): the
 * summary can be copied or saved as a .txt, and closing it unsaved asks
 * first. The creation's progress runs at the top of the summary.
 */
import type { SiteType, Task } from '~/types'
import type { SiteSummary } from '~/utils/newSite'

const open = defineModel<boolean>('open', { default: false })
const emit = defineEmits<{ created: [] }>()
const { t } = useI18n()
const api = useApi()
const toast = useToast()
const confirm = useConfirm()
const { tasks } = useTasks()
const caps = useCapabilities()
const { copy } = useClipboard({ legacy: true })

const withDBTypes: SiteType[] = ['wordpress', 'woocommerce', 'laravel']
const optionalDBTypes: SiteType[] = ['static', 'php']

const fresh = () => ({
  domain: '',
  type: 'wordpress' as SiteType,
  profile: 'auto',
  php_version: 'auto',
  title: '',
  admin_email: '',
  admin_user: 'admin',
  admin_password: '',
  proxy_upstream: '',
  with_db: false,
  db_name: '',
  db_user: '',
  db_password: generatePassword()
})
const state = reactive(fresh())
// The names follow the domain until they are edited.
const edited = reactive({ name: false, user: false })
const show = reactive({ admin: false, db: false })
const busy = ref(false)
const tried = ref(false)
const serverErrors = ref<Record<string, string>>({})

const phase = ref<'form' | 'summary'>('form')
const created = ref<{ id: number, domain: string, task: Task, summary: SiteSummary } | null>(null)
const saved = ref(false)
const copiedAll = ref(false)

watch(open, (o) => {
  if (!o || phase.value === 'form') return
  // Opened again after a creation: a new form.
  phase.value = 'form'
  created.value = null
})

const isWP = computed(() => state.type === 'wordpress' || state.type === 'woocommerce')
const hasPHP = computed(() => state.type !== 'static' && state.type !== 'proxy')
const dbAlways = computed(() => withDBTypes.includes(state.type))
const dbOptional = computed(() => optionalDBTypes.includes(state.type))
const wantsDB = computed(() => dbAlways.value || (dbOptional.value && state.with_db))

watch(() => state.domain, (d) => {
  const proposed = proposeDBName(d)
  if (!edited.name) state.db_name = proposed
  if (!edited.user) state.db_user = proposed
})
watch(() => [state.domain, state.type, state.with_db, state.db_name, state.db_user, state.db_password], () => { serverErrors.value = {} })

const typeDefs = computed(() => SITE_TYPES.map(v => ({ value: v, label: t(`site.type.${v}`), desc: t(`newSite.typeHint.${v}`) })))
const profileItems = computed(() => [
  { label: t('site.profileRecommended', { p: t(`site.profile.${state.type === 'woocommerce' ? 'commerce' : 'balanced'}`) }), value: 'auto' },
  ...(['balanced', 'commerce', 'maximum'] as const).map(v => ({ label: `${t(`site.profile.${v}`)} — ${t(`site.profileHint.${v}`)}`, value: v }))
])
// The versions installed on the server, as its agent reports them.
const phpItems = computed(() => [
  { label: caps.value?.default_php ? `${t('site.phpDefault')} · PHP ${caps.value.default_php}` : t('site.phpDefault'), value: 'auto' },
  ...(caps.value?.php_versions ?? []).map(v => ({ label: `PHP ${v}`, value: v }))
])

// ---- checks (the server's rules; it checks again) ----
const nameProblem = (n: string) => {
  if (!n) return t('newSite.nameRequired')
  if (!DB_NAME_RE.test(n)) return t('dbs.nameRule')
  if (reservedDBName(n)) return t('newSite.nameReserved')
  return undefined
}
const errors = computed(() => {
  const e: Record<string, string | undefined> = {}
  if (!state.domain.trim()) e.domain = t('newSite.domainRequired')
  if (isWP.value) {
    if (!/^[^\s@]+@[^\s@]+$/.test(state.admin_email.trim())) e.admin_email = t('newSite.emailRequired')
    if (!state.admin_user.trim()) e.admin_user = t('newSite.adminUserRequired')
    if (state.admin_password.length < 12) e.admin_password = t('auth.min12')
  }
  if (state.type === 'proxy' && !state.proxy_upstream.trim()) e.proxy_upstream = t('newSite.upstreamRequired')
  if (wantsDB.value) {
    e.database_name = nameProblem(state.db_name)
    e.database_user = nameProblem(state.db_user)
    const issue = dbPasswordIssue(state.db_password)
    if (issue) e.database_password = t(issue)
  }
  return e
})
const shownError = (field: string) => serverErrors.value[field] || (tried.value ? errors.value[field] : undefined) ||
  (field.startsWith('database_') && field !== 'database_password' && (field === 'database_name' ? state.db_name : state.db_user) ? errors.value[field] : undefined)
const valid = computed(() => Object.values(errors.value).every(v => !v))

// What the site will get, in one line (the artboard's).
const willCreate = computed(() => {
  const parts = [t('newSite.willUser')]
  if (wantsDB.value && state.db_name) parts.push(t('newSite.willDatabase', { name: state.db_name }))
  parts.push(t('newSite.willCert'))
  const p = state.profile === 'auto' ? (state.type === 'woocommerce' ? 'commerce' : 'balanced') : state.profile
  parts.push(t('newSite.willProfile', { profile: t(`site.profile.${p}`) }))
  if (state.type !== 'proxy') parts.push(t('newSite.willBackups'))
  return `${t('newSite.willCreate')} ${parts.join(' · ')}`
})

async function submit() {
  tried.value = true
  if (!valid.value || busy.value) return
  const body: Record<string, unknown> = { domain: state.domain.trim().toLowerCase(), type: state.type }
  if (state.profile !== 'auto') body.profile = state.profile
  if (hasPHP.value && state.php_version !== 'auto') body.php_version = state.php_version
  if (isWP.value) Object.assign(body, { title: state.title, admin_email: state.admin_email.trim(), admin_user: state.admin_user.trim(), admin_password: state.admin_password })
  if (state.type === 'proxy') body.proxy_upstream = state.proxy_upstream.trim()
  if (dbOptional.value) body.create_database = state.with_db
  if (wantsDB.value) Object.assign(body, { database_name: state.db_name, database_user: state.db_user, database_password: state.db_password })
  busy.value = true
  try {
    const res = await api.post<{ site: { id: number, domain: string }, task: Task, summary: SiteSummary }>('/api/sites', body)
    created.value = { id: res.site.id, domain: res.site.domain, task: res.task, summary: res.summary }
    saved.value = !res.summary.database
    copiedAll.value = false
    seen.value = []
    phase.value = 'summary'
    tried.value = false
    Object.assign(state, fresh())
    Object.assign(edited, { name: false, user: false })
    emit('created')
  } catch (e) {
    const fields = isApiError(e) && e.data && typeof e.data.fields === 'object' ? e.data.fields as Record<string, string> : null
    if (fields && Object.keys(fields).length) {
      // A name in use (409) is said in the reader's language; other field
      // problems are the server's words.
      const taken = isApiError(e) && e.status === 409
      serverErrors.value = Object.fromEntries(Object.entries(fields).map(([k, v]) => [k,
        taken && k === 'database_name' ? t('newSite.nameTaken', { name: state.db_name })
          : taken && k === 'database_user' ? t('newSite.nameTaken', { name: state.db_user }) : v]))
    }
    toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
  } finally {
    busy.value = false
  }
}

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
const steps = computed(() => seen.value.map((what, i) => {
  const last = i === seen.value.length - 1
  const st = !last ? 'done' : taskState.value === 'succeeded' ? 'done' : taskState.value === 'failed' ? 'failed' : 'running'
  return { n: String(i + 1).padStart(2, '0'), what, st }
}))
const stateLabel = (st: string) => t(`newSite.step.${st}`)
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

// Reloading or leaving the page would lose the password too: the browser
// asks first while the summary is open and unsaved.
useEventListener(window, 'beforeunload', (ev: BeforeUnloadEvent) => {
  if (open.value && phase.value === 'summary' && !saved.value) {
    ev.preventDefault()
    ev.returnValue = ''
  }
})

async function close() {
  if (phase.value === 'summary' && !saved.value) {
    const ok = await confirm({ title: t('newSite.closeTitle'), description: t('newSite.closeHint'), confirmLabel: t('newSite.closeAnyway'), danger: true })
    if (!ok) return
  }
  open.value = false
}
function openSite() {
  const id = created.value?.id
  void close().then(() => {
    if (!open.value && id) void navigateTo(`/sites/${id}`)
  })
}
</script>

<template>
  <UModal
    v-model:open="open"
    :dismissible="phase === 'form'"
    :close="false"
    :title="t('site.new')"
    :description="t('site.newHint')"
    :ui="{ content: 'sm:max-w-[640px]', header: 'p-0 min-h-0', body: 'p-0 sm:p-0', footer: 'px-5 py-4 sm:px-6' }"
  >
    <template #header>
      <div class="flex w-full items-start justify-between gap-4 px-5 pt-5 pb-4 sm:px-6">
        <div class="flex min-w-0 flex-col gap-1.5">
          <h2 class="m-0 text-[1.375rem] leading-tight font-semibold tracking-[-0.015em] text-highlighted [overflow-wrap:anywhere]">
            <template v-if="phase === 'form'">{{ t('newSite.title') }} {{ t('newSite.titleAccent') }}</template>
            <template v-else>{{ created?.domain }}</template>
          </h2>
          <p class="m-0 text-[13px] leading-normal text-muted">{{ phase === 'form' ? t('site.newHint') : t('newSite.summaryHint') }}</p>
        </div>
        <UButton color="neutral" variant="ghost" square icon="i-cg-x" :aria-label="t('newSite.close')" @click="close" />
      </div>
    </template>

    <template #body>
      <!-- The form -->
      <form v-if="phase === 'form'" id="create-site" class="flex flex-col gap-[18px] px-5 py-5 sm:px-6" novalidate @submit.prevent="submit">
        <UFormField :label="t('site.domain')" required :error="shownError('domain')">
          <UInput v-model="state.domain" placeholder="example.com" class="w-full" :ui="{ base: 'font-mono' }" autofocus autocomplete="off" spellcheck="false" />
        </UFormField>

        <div class="flex flex-col gap-2">
          <span id="ns-type" class="text-sm font-medium text-highlighted">{{ t('site.typeLabel') }}</span>
          <div role="radiogroup" aria-labelledby="ns-type" class="cg-grid grid-cols-2 sm:grid-cols-3">
            <button
              v-for="d in typeDefs"
              :key="d.value"
              type="button"
              role="radio"
              :aria-checked="state.type === d.value"
              class="flex min-h-[68px] min-w-0 flex-col items-start gap-1 p-3 text-start transition-colors"
              :class="state.type === d.value ? 'bg-cg-blue-tint text-highlighted shadow-[inset_0_0_0_2px_var(--cg-blue)]' : 'bg-default text-highlighted hover:bg-elevated'"
              @click="state.type = d.value"
            >
              <span class="text-sm font-semibold">{{ d.label }}</span>
              <span class="text-[13px] text-muted">{{ d.desc }}</span>
            </button>
          </div>
        </div>

        <UFormField v-if="state.type === 'proxy'" :label="t('site.upstream')" required :error="shownError('proxy_upstream')" :help="t('newSite.upstreamHelp')">
          <UInput v-model="state.proxy_upstream" placeholder="http://127.0.0.1:3000" class="w-full" :ui="{ base: 'font-mono' }" />
        </UFormField>

        <div class="grid gap-4 sm:grid-cols-2">
          <UFormField v-if="hasPHP" label="PHP">
            <USelect v-model="state.php_version" :items="phpItems" class="w-full" />
          </UFormField>
          <UFormField :label="t('site.profileLabel')" :class="hasPHP ? '' : 'sm:col-span-2'">
            <USelect v-model="state.profile" :items="profileItems" class="w-full" />
          </UFormField>
        </div>

        <section v-if="isWP" class="flex flex-col gap-4 border-t border-default pt-4" :aria-label="t('newSite.wpPart')">
          <h3 class="m-0 text-[15px] font-semibold text-highlighted">{{ t('newSite.wpPart') }}</h3>
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

        <section v-if="state.type !== 'proxy'" class="flex flex-col gap-4 border-t border-default pt-4" :aria-label="t('newSite.dbPart')">
          <h3 class="m-0 text-[15px] font-semibold text-highlighted">{{ t('newSite.dbPart') }}</h3>
          <USwitch v-if="dbOptional" v-model="state.with_db" :label="t('newSite.withDatabase')" :description="t('newSite.withDatabaseHint')" />
          <p v-else class="m-0 text-[13px] leading-normal text-muted">{{ t('newSite.dbAlways', { type: t(`site.type.${state.type}`) }) }}</p>
          <template v-if="wantsDB">
            <div class="grid gap-4 sm:grid-cols-2">
              <UFormField :label="t('newSite.dbName')" required :error="shownError('database_name')">
                <UInput v-model="state.db_name" class="w-full" :ui="{ base: 'font-mono' }" autocomplete="off" spellcheck="false" @input="edited.name = true" />
              </UFormField>
              <UFormField :label="t('newSite.dbUser')" required :error="shownError('database_user')">
                <UInput v-model="state.db_user" class="w-full" :ui="{ base: 'font-mono' }" autocomplete="off" spellcheck="false" @input="edited.user = true" />
              </UFormField>
            </div>
            <p class="m-0 -mt-2 text-[13px] leading-normal text-muted">{{ t('dbs.nameHint') }}</p>
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
        </section>

        <p class="mono m-0 bg-elevated px-3 py-2.5 text-xs leading-relaxed text-muted [overflow-wrap:anywhere]">{{ willCreate }}</p>
      </form>

      <!-- The summary -->
      <div v-else-if="created" class="flex flex-col gap-5 px-5 py-5 sm:px-6" data-testid="site-summary">
        <ol class="cg-list" :aria-label="t('newSite.progress')">
          <li v-for="s in steps" :key="s.n" class="flex items-center gap-3.5 px-0.5 py-2.5">
            <span class="mono min-w-[22px] text-[13px] text-cg-blue">{{ s.n }}</span>
            <span class="min-w-0 flex-1 text-sm text-highlighted [overflow-wrap:anywhere]">{{ s.what }}</span>
            <span class="shrink-0 text-end text-[13px] font-medium" :class="stateColor(s.st)">{{ stateLabel(s.st) }}</span>
          </li>
          <li v-if="!steps.length" class="flex items-center gap-3.5 px-0.5 py-2.5">
            <span class="mono min-w-[22px] text-[13px] text-cg-blue">01</span>
            <span class="min-w-0 flex-1 text-sm text-highlighted">{{ t('newSite.queued') }}</span>
            <span class="shrink-0 text-[13px] font-medium text-cg-blue">{{ stateLabel('running') }}</span>
          </li>
        </ol>
        <UAlert v-if="taskState === 'failed'" color="error" variant="subtle" icon="i-cg-x-circle" :title="t('newSite.failed')" :description="task?.error" />

        <dl class="cg-grid m-0 grid-cols-1 sm:grid-cols-[minmax(0,180px)_minmax(0,1fr)]">
          <template v-for="(row, i) in rows" :key="i">
            <dt v-if="row.part" class="bg-elevated px-3 py-2 text-[13px] font-semibold text-highlighted sm:col-span-2">{{ row.part }}</dt>
            <template v-else>
              <dt class="bg-default px-3 pt-2.5 text-[13px] text-muted sm:py-2.5">{{ row.label }}</dt>
              <dd class="m-0 flex min-w-0 items-center gap-2 bg-default px-3 pb-2.5 sm:py-2">
                <a v-if="row.href" :href="row.href" target="_blank" rel="noopener" class="mono min-w-0 flex-1 text-[13px] text-highlighted underline-offset-2 [overflow-wrap:anywhere] hover:underline">{{ row.value }}</a>
                <a v-else-if="row.panel" :href="row.value" class="mono min-w-0 flex-1 text-[13px] text-highlighted underline-offset-2 [overflow-wrap:anywhere] hover:underline" @click.prevent="openSite">{{ row.value }}</a>
                <span v-else class="mono min-w-0 flex-1 text-[13px] text-highlighted [overflow-wrap:anywhere]">{{ row.value }}</span>
                <CopyButton v-if="row.copy" :value="row.value ?? ''" />
              </dd>
            </template>
          </template>
        </dl>
        <UAlert v-if="created.summary.database" color="warning" variant="subtle" icon="i-cg-key" :title="t('newSite.onceTitle')" :description="t('newSite.onceHint')" />
      </div>
    </template>

    <template #footer>
      <div v-if="phase === 'form'" class="flex w-full flex-wrap justify-end gap-2.5">
        <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="close" />
        <UButton type="submit" form="create-site" color="primary" variant="solid" :label="t('site.create')" :loading="busy" />
      </div>
      <div v-else class="flex w-full flex-wrap items-center justify-end gap-2.5">
        <UButton color="neutral" variant="outline" icon="i-cg-download" :label="t('newSite.download')" data-testid="summary-download" @click="download" />
        <UButton color="neutral" variant="outline" :icon="copiedAll ? 'i-cg-check' : 'i-cg-copy'" :label="copiedAll ? t('common.copied') : t('common.copy')" data-testid="summary-copy" @click="copyAll" />
        <span class="hidden flex-1 sm:block" />
        <UButton color="neutral" variant="ghost" :label="t('newSite.close')" data-testid="summary-close" @click="close" />
        <UButton color="primary" variant="solid" :label="t('newSite.openSite')" trailing-icon="i-cg-arrow-right" @click="openSite" />
      </div>
    </template>
  </UModal>
</template>
