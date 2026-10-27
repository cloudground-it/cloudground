<script setup lang="ts">
/**
 * A site's Database tab: its databases (size, tables, a way into Quarry)
 * and its database users with their privileges on each database. Nothing
 * is created by default for a static or plain PHP site; WordPress,
 * WooCommerce and Laravel start with one database and one user, the
 * primary ones their configuration uses. Tables, SQL, import and export are
 * Quarry's, a service of its own (/quarry): its "Open in" button asks
 * the API for a one-time handoff and opens it in a new tab. The WordPress
 * tools are this route's child page.
 */
import type { DropdownMenuItem } from '@nuxt/ui'
import type { DBGrant, DBGrantLevel, SiteDatabases, SiteDBUser } from '~/types/dbstudio'

type Level = DBGrantLevel | 'none'

const { t, locale } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site } = useCurrentSite()
const route = useRoute()
// DB Studio's pages are children of this route: they render in place of the overview.
const child = computed(() => route.name !== 'sites-id-database')

const base = computed(() => `/api/sites/${site.value.id}`)
const { data, pending, refresh } = useLive(() => api.get<SiteDatabases>(`${base.value}/databases`), {
  on: { types: ['databases.changed'], sites: () => site.value.id },
  fallbackMs: 30000
})
const databases = computed(() => data.value?.databases ?? [])
const users = computed(() => data.value?.users ?? [])
// External servers (#30): picked when a database or a user is made, shown
// beside what is on one, with where an application connects.
const available = computed(() => data.value?.available_servers ?? [])
const serverItems = computed(() => [{ value: 0, label: t('dbs.thisServer') }, ...available.value.map(s => ({ value: s.id, label: s.name }))])
const serverName = (id: number) => id === 0 ? t('dbs.thisServer') : (data.value?.servers ?? []).find(s => s.id === id)?.name ?? `#${id}`
const external = computed(() => (data.value?.servers ?? []).filter(s => s.id !== 0))

const NAME_RE = /^[a-z][a-z0-9_]{2,47}$/
const nameError = (n: string) => n && !NAME_RE.test(n) ? t('dbs.nameRule') : undefined
// A chosen password follows the server's one rule (utils/newSite.ts).
const passwordError = (pw: string) => {
  const issue = pw ? dbPasswordIssue(pw) : undefined
  return issue ? t(issue) : undefined
}

// DB Studio opens in its own tab, signed in through a one-time handoff
// bound to this panel session. The tab is opened at the click, before the
// request, so the browser does not take it for a pop-up.
const toast = useToast()
async function openStudio(name: string) {
  const win = window.open('about:blank', '_blank')
  try {
    const r = await api.post<{ url: string }>(`${base.value}/quarry/handoff`, { database: name })
    if (win) win.location.href = r.url
    else window.location.href = r.url
  } catch (e) {
    win?.close()
    toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
  }
}
const wpTools = computed(() => isWordPress(site.value) && databases.value.some(d => d.primary))
const size = (n: number) => n < 0 ? '—' : fmtBytes(n, locale.value)
const count = (n: number) => n < 0 ? '—' : fmtNumber(n, undefined, locale.value)

// ---- databases ----
const newDB = ref('')
const newDBServer = ref(0)
const addingDB = ref(false)
function startAddDB() {
  newDB.value = ''
  newDBServer.value = 0
  addingDB.value = true
}
async function createDB() {
  const name = newDB.value.trim()
  if (!NAME_RE.test(name)) return
  if (await run(() => api.post(`${base.value}/databases`, { name, server_id: newDBServer.value }), t('dbs.dbCreating', { name }))) {
    newDB.value = ''
    addingDB.value = false
  }
}
// Who reaches each database, read off the users' grants: the Databases
// table shows it next to the database, as the Users table shows the
// databases next to each user.
const accessOf = (db: string) => users.value.flatMap(u => (u.grants ?? []).filter(g => g.database === db).map(g => ({ user: u.name, level: g.level })))
const dbColumns = computed(() => [
  { id: 'name', header: t('db.name') },
  { id: 'access', header: t('ux.dbs.access'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { id: 'tables', header: t('dbs.tables'), meta: { class: { th: 'hidden sm:table-cell text-end', td: 'hidden sm:table-cell text-end' } } },
  { id: 'size', header: t('dbs.size'), meta: { class: { th: 'hidden sm:table-cell text-end', td: 'hidden sm:table-cell text-end' } } },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])
const dbMenu = (name: string, primary: boolean): DropdownMenuItem[][] => [[
  { label: primary ? t('dbs.primaryKept') : t('common.delete'), icon: 'i-cg-trash', color: 'error', disabled: primary, onSelect: () => void dropDB(name) }
]]
async function dropDB(name: string) {
  const ok = await confirm({ title: t('dbs.dropTitle', { name }), description: t('dbs.dropHint'), danger: true, typed: name, confirmLabel: t('common.delete') })
  if (ok) await run(() => api.del(`${base.value}/databases/${encodeURIComponent(name)}?confirm=${encodeURIComponent(name)}`), t('dbs.dbDropping', { name }))
}

// ---- users ----
const levelItems = computed(() => [
  { value: 'none', label: t('dbs.levelNone') },
  { value: 'read', label: t('dbs.levelRead') },
  { value: 'all', label: t('dbs.levelAll') }
])
const levelLabel = (l: DBGrantLevel) => l === 'all' ? t('dbs.levelAll') : t('dbs.levelRead')
const toGrants = (m: Record<string, Level>): DBGrant[] =>
  Object.entries(m).filter(([, l]) => l !== 'none').map(([database, l]) => ({ database, level: l as DBGrantLevel }))
const levelsOf = (u?: SiteDBUser): Record<string, Level> => {
  const out: Record<string, Level> = {}
  for (const d of databases.value) out[d.name] = (u?.grants ?? []).find(g => g.database === d.name)?.level ?? 'none'
  return out
}

const newUser = reactive({ name: '', password: '', server: 0, levels: {} as Record<string, Level> })
// A user is on one server and reaches only the databases that are on it.
const databasesOn = (server: number) => databases.value.filter(d => (d.server_id ?? 0) === server)
watch(databases, () => {
  newUser.levels = { ...levelsOf(), ...Object.fromEntries(Object.entries(newUser.levels).filter(([k]) => databases.value.some(d => d.name === k))) }
}, { immediate: true })
const newUserValid = computed(() => NAME_RE.test(newUser.name) && !passwordError(newUser.password))
const addingUser = ref(false)
function startAddUser() {
  newUser.name = ''
  newUser.password = ''
  newUser.server = 0
  newUser.levels = levelsOf()
  addingUser.value = true
}
const userColumns = computed(() => [
  { id: 'name', header: t('db.user') },
  { id: 'grants', header: t('ux.dbs.access'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])
const userMenu = (u: SiteDBUser): DropdownMenuItem[][] => [
  [{ label: t('dbs.resetPassword'), icon: 'i-cg-key', onSelect: () => startReset(u) }],
  [{ label: u.primary ? t('dbs.primaryUserKept') : t('common.delete'), icon: 'i-cg-trash', color: 'error', disabled: u.primary, onSelect: () => void dropUser(u.name) }]
]

// The password the server generated, shown once.
const shown = ref<{ user: string, password: string } | null>(null)

async function createUser() {
  const name = newUser.name.trim()
  let res: { password?: string } | undefined
  const ok = await run(async () => {
    res = await api.post<{ password?: string }>(`${base.value}/database-users`, { name, password: newUser.password, server_id: newUser.server, grants: toGrants(newUser.levels) })
  }, t('dbs.userCreated', { name }))
  if (!ok) return
  addingUser.value = false
  if (res?.password) shown.value = { user: name, password: res.password }
  newUser.name = ''
  newUser.password = ''
  newUser.levels = levelsOf()
  await refresh()
}

async function dropUser(name: string) {
  const ok = await confirm({ title: t('dbs.dropUserTitle', { name }), description: t('dbs.dropUserHint'), danger: true, typed: name, confirmLabel: t('common.delete') })
  if (ok && await run(() => api.del(`${base.value}/database-users/${encodeURIComponent(name)}?confirm=${encodeURIComponent(name)}`), t('dbs.userDeleted', { name }))) await refresh()
}

// Privileges of an existing user.
const editing = ref<SiteDBUser | null>(null)
const editable = computed(() => databasesOn(editing.value?.server_id ?? 0))
const editLevels = ref<Record<string, Level>>({})
function editGrants(u: SiteDBUser) {
  editing.value = u
  editLevels.value = levelsOf(u)
}
async function saveGrants() {
  const u = editing.value
  if (!u) return
  if (await run(() => api.put(`${base.value}/database-users/${encodeURIComponent(u.name)}/grants`, { grants: toGrants(editLevels.value) }), t('dbs.grantsSaved'))) {
    editing.value = null
    await refresh()
  }
}

// A new password: generated unless one is chosen.
const resetting = ref<SiteDBUser | null>(null)
const resetMode = ref<'generate' | 'choose'>('generate')
const resetPassword = ref('')
const resetModes = computed(() => [
  { value: 'generate', label: t('dbs.generate') },
  { value: 'choose', label: t('dbs.choose') }
])
function startReset(u: SiteDBUser) {
  resetting.value = u
  resetMode.value = 'generate'
  resetPassword.value = ''
}
const resetValid = computed(() => resetMode.value === 'generate' || (resetPassword.value !== '' && !passwordError(resetPassword.value)))
async function savePassword() {
  const u = resetting.value
  if (!u) return
  const password = resetMode.value === 'choose' ? resetPassword.value : ''
  let res: { password?: string } | undefined
  const ok = await run(async () => {
    res = await api.post<{ password?: string }>(`${base.value}/database-users/${encodeURIComponent(u.name)}/password`, { password })
  }, t('dbs.passwordChanged'))
  if (!ok) return
  resetting.value = null
  resetPassword.value = ''
  if (res?.password) shown.value = { user: u.name, password: res.password }
}
const wpPrimary = (u: SiteDBUser) => u.primary && isWordPress(site.value)
</script>

<template>
  <NuxtPage v-if="child" />
  <template v-else>
    <UAlert v-if="data?.stats_error" color="warning" variant="subtle" icon="i-cg-alert-triangle" :title="t('dbs.statsError')" :description="data.stats_error" />

    <PanelSection :title="t('dbs.databases')" :description="t('dbs.databasesHint')" flush>
      <template #actions>
        <UButton v-if="wpTools" color="neutral" variant="outline" icon="i-cg-browser" :label="t('studio.wpTools')" :to="`/sites/${site.id}/database/wordpress`" />
        <UButton v-if="canManage" icon="i-cg-plus" :label="t('ux.dbs.addDatabase')" @click="startAddDB" />
      </template>

      <USkeleton v-if="pending && !data" class="m-4 h-24" />
      <UEmpty
        v-else-if="!databases.length"
        icon="i-cg-database"
        :title="t('dbs.noDatabases')"
        :description="t('dbs.noDatabasesHint')"
        :actions="canManage ? [{ label: t('ux.dbs.addDatabase'), icon: 'i-cg-plus', onClick: startAddDB }] : []"
      />
      <UTable v-else :data="databases" :columns="dbColumns">
        <template #name-cell="{ row: { original: d } }">
          <span class="flex items-center gap-2">
            <UIcon name="i-cg-database" class="size-4 shrink-0 text-muted" />
            <span class="mono text-[13px] font-semibold text-highlighted">{{ d.name }}</span>
            <CopyButton :value="d.name" />
            <UTooltip v-if="d.primary" :text="t('dbs.primaryHint')">
              <UBadge color="info" variant="subtle" :label="t('dbs.primary')" />
            </UTooltip>
            <UBadge v-if="d.server_id" color="neutral" variant="outline" icon="i-cg-database" :label="serverName(d.server_id)" />
          </span>
          <span class="mt-1 flex flex-wrap gap-1.5 md:hidden">
            <UBadge v-for="x in accessOf(d.name)" :key="x.user" :color="x.level === 'all' ? 'info' : 'neutral'" variant="outline" size="sm" class="mono" :label="`${x.user} · ${levelLabel(x.level)}`" />
          </span>
        </template>
        <template #access-cell="{ row: { original: d } }">
          <span v-if="accessOf(d.name).length" class="flex flex-wrap gap-1.5">
            <UBadge v-for="a in accessOf(d.name)" :key="a.user" :color="a.level === 'all' ? 'info' : 'neutral'" variant="outline" class="mono" :label="`${a.user} · ${levelLabel(a.level)}`" />
          </span>
          <span v-else class="text-[13px] text-muted">{{ t('ux.dbs.noUsersOn') }}</span>
        </template>
        <template #tables-cell="{ row: { original: d } }">
          <span class="mono">{{ count(d.tables) }}</span>
        </template>
        <template #size-cell="{ row: { original: d } }">
          <span class="mono">{{ size(d.size_bytes) }}</span>
        </template>
        <template #actions-cell="{ row: { original: d } }">
          <span class="flex items-center justify-end gap-1">
            <UButton size="sm" color="neutral" variant="outline" icon="i-cg-external-link" :label="t('dbs.openStudio')" :aria-label="t('dbs.openStudio')" :ui="{ label: 'hidden sm:inline' }" @click="openStudio(d.name)" />
            <UDropdownMenu v-if="canManage" :items="dbMenu(d.name, d.primary)" :content="{ align: 'end' }">
              <UButton size="sm" color="neutral" variant="ghost" square icon="i-cg-more-horizontal" :aria-label="t('ux.common.moreFor', { name: d.name })" />
            </UDropdownMenu>
          </span>
        </template>
      </UTable>
      <template #footer>
        <div class="flex flex-col gap-1">
          <p class="m-0 text-[13px] text-muted">
            {{ t('studio.panelHint') }}
          </p>
          <p v-for="s in external" :key="s.id" class="m-0 text-[13px] text-muted">
            {{ t('dbs.connectAt', { server: s.name, host: `${s.host}:${s.port}`, tls: s.tls === 'required' ? t('dbs.connectTls') : t('dbs.connectNoTls') }) }}
          </p>
        </div>
      </template>
    </PanelSection>

    <PanelSection :title="t('dbs.users')" :description="t('dbs.usersHint')" flush>
      <template v-if="canManage" #actions>
        <UButton icon="i-cg-user-plus" :label="t('ux.dbs.addUser')" @click="startAddUser" />
      </template>

      <USkeleton v-if="pending && !data" class="m-4 h-24" />
      <UEmpty
        v-else-if="!users.length"
        icon="i-cg-user"
        :title="t('dbs.noUsers')"
        :description="t('dbs.usersHint')"
        :actions="canManage ? [{ label: t('ux.dbs.addUser'), icon: 'i-cg-user-plus', onClick: startAddUser }] : []"
      />
      <UTable v-else :data="users" :columns="userColumns">
        <template #name-cell="{ row: { original: u } }">
          <span class="flex items-center gap-2">
            <UIcon name="i-cg-user" class="size-4 shrink-0 text-muted" />
            <span class="mono text-[13px] font-semibold text-highlighted">{{ u.name }}</span>
            <CopyButton :value="u.name" />
            <UTooltip v-if="u.primary" :text="t('dbs.primaryUserHint')">
              <UBadge color="info" variant="subtle" :label="t('dbs.primary')" />
            </UTooltip>
            <UBadge v-if="u.server_id" color="neutral" variant="outline" icon="i-cg-database" :label="serverName(u.server_id)" />
          </span>
          <span class="mt-1 flex flex-wrap gap-1.5 md:hidden">
            <UBadge v-for="g in u.grants ?? []" :key="g.database" :color="g.level === 'all' ? 'info' : 'neutral'" variant="outline" size="sm" class="mono" :label="`${g.database} · ${levelLabel(g.level)}`" />
          </span>
        </template>
        <template #grants-cell="{ row: { original: u } }">
          <span v-if="(u.grants ?? []).length" class="flex flex-wrap gap-1.5">
            <UBadge v-for="g in u.grants ?? []" :key="g.database" :color="g.level === 'all' ? 'info' : 'neutral'" variant="outline" class="mono" :label="`${g.database} · ${levelLabel(g.level)}`" />
          </span>
          <span v-else class="text-[13px] text-muted">{{ t('dbs.noGrants') }}</span>
        </template>
        <template #actions-cell="{ row: { original: u } }">
          <span v-if="canManage" class="flex items-center justify-end gap-1">
            <UButton size="sm" color="neutral" variant="outline" icon="i-cg-sliders" :label="t('dbs.editGrants')" :aria-label="t('dbs.editGrants')" :ui="{ label: 'hidden sm:inline' }" @click="editGrants(u)" />
            <UDropdownMenu :items="userMenu(u)" :content="{ align: 'end' }">
              <UButton size="sm" color="neutral" variant="ghost" square icon="i-cg-more-horizontal" :aria-label="t('ux.common.moreFor', { name: u.name })" />
            </UDropdownMenu>
          </span>
        </template>
      </UTable>
    </PanelSection>

    <UModal v-model:open="addingDB" :title="t('dbs.newDatabase')" :description="t('dbs.databasesHint')">
      <template #body>
        <UForm id="add-db" :state="{ name: newDB }" @submit="createDB">
          <UFormField :label="t('db.name')" :help="t('dbs.nameHint')" :error="nameError(newDB)" name="name">
            <UInput v-model="newDB" class="w-full font-mono" placeholder="shop_data" autocomplete="off" spellcheck="false" autofocus />
          </UFormField>
          <UFormField v-if="available.length" :label="t('dbs.server')" :help="t('dbs.serverHelp')" class="mt-4" name="server">
            <USelect v-model="newDBServer" :items="serverItems" class="w-full sm:w-64" />
          </UFormField>
        </UForm>
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="addingDB = false" />
          <UButton color="primary" variant="solid" type="submit" form="add-db" icon="i-cg-plus" :label="t('dbs.createDatabase')" :disabled="!NAME_RE.test(newDB)" :loading="busy" />
        </div>
      </template>
    </UModal>

    <UModal v-model:open="addingUser" :title="t('dbs.newUser')" :description="t('dbs.usersHint')">
      <template #body>
        <UForm id="add-user" :state="newUser" class="flex flex-col gap-4" @submit="createUser">
          <UFormField :label="t('db.user')" :help="t('dbs.nameHint')" :error="nameError(newUser.name)" name="name">
            <UInput v-model="newUser.name" class="w-full font-mono" placeholder="reporting" autocomplete="off" spellcheck="false" autofocus />
          </UFormField>
          <UFormField :label="t('common.password')" :hint="t('common.optional')" :help="t('dbs.passwordHelp')" :error="passwordError(newUser.password)" name="password">
            <UInput v-model="newUser.password" type="password" autocomplete="new-password" class="w-full font-mono" />
          </UFormField>
          <UFormField v-if="available.length" :label="t('dbs.server')" :help="t('dbs.userServerHelp')" name="server">
            <USelect v-model="newUser.server" :items="serverItems" class="w-full sm:w-64" />
          </UFormField>
          <UFormField v-if="databasesOn(newUser.server).length" :label="t('dbs.privileges')">
            <div class="flex flex-col divide-y divide-default border-y border-default">
              <div v-for="d in databasesOn(newUser.server)" :key="d.name" class="flex flex-wrap items-center justify-between gap-3 py-2">
                <span class="mono text-[13px] text-highlighted">{{ d.name }}</span>
                <USelect v-model="newUser.levels[d.name]" :items="levelItems" size="sm" class="w-44" :aria-label="t('ux.dbs.levelOn', { name: d.name })" />
              </div>
            </div>
          </UFormField>
        </UForm>
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="addingUser = false" />
          <UButton color="primary" variant="solid" type="submit" form="add-user" icon="i-cg-user-plus" :label="t('dbs.createUser')" :disabled="!newUserValid" :loading="busy" />
        </div>
      </template>
    </UModal>

    <UModal :open="!!editing" :title="editing ? t('dbs.grantsTitle', { name: editing.name }) : ''" @update:open="v => { if (!v) editing = null }">
      <template #body>
        <p v-if="!editable.length" class="m-0 text-sm text-muted">
          {{ t('dbs.noDatabases') }}
        </p>
        <div v-else class="flex flex-col divide-y divide-default border-y border-default">
          <div v-for="d in editable" :key="d.name" class="flex flex-wrap items-center justify-between gap-3 py-2">
            <span class="mono text-[13px] text-highlighted">{{ d.name }}</span>
            <USelect v-model="editLevels[d.name]" :items="levelItems" size="sm" class="w-44" :aria-label="t('ux.dbs.levelOn', { name: d.name })" />
          </div>
        </div>
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="editing = null" />
          <UButton color="primary" variant="solid" :label="t('common.save')" :loading="busy" @click="saveGrants" />
        </div>
      </template>
    </UModal>

    <UModal :open="!!resetting" :title="resetting ? t('dbs.resetTitle', { name: resetting.name }) : ''" @update:open="v => { if (!v) resetting = null }">
      <template #body>
        <div class="flex flex-col gap-4">
          <URadioGroup v-model="resetMode" :items="resetModes" />
          <UFormField v-if="resetMode === 'choose'" :label="t('common.password')" :hint="t('auth.min12')" :error="passwordError(resetPassword)">
            <UInput v-model="resetPassword" type="password" autocomplete="new-password" class="w-full font-mono" />
          </UFormField>
          <UAlert v-if="resetting && wpPrimary(resetting)" color="neutral" variant="subtle" icon="i-cg-info" :description="t('dbs.resetWordPress')" />
          <UAlert v-else-if="resetting?.primary" color="warning" variant="subtle" icon="i-cg-alert-triangle" :description="t('dbs.resetPrimary')" />
        </div>
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="resetting = null" />
          <UButton color="primary" variant="solid" :label="t('dbs.resetPassword')" :disabled="!resetValid" :loading="busy" @click="savePassword" />
        </div>
      </template>
    </UModal>

    <UModal :open="!!shown" :title="t('dbs.passwordTitle')" :dismissible="false" @update:open="v => { if (!v) shown = null }">
      <template #body>
        <div class="flex flex-col gap-4">
          <UAlert color="warning" variant="subtle" icon="i-cg-alert-triangle" :description="t('dbs.passwordOnce')" />
          <SummaryList :rows="[{ k: t('db.user'), v: shown?.user ?? '', mono: true, copy: true }, { k: t('common.password'), v: shown?.password ?? '', mono: true, copy: true }]" />
        </div>
      </template>
      <template #footer>
        <div class="flex w-full justify-end">
          <UButton color="primary" variant="solid" :label="t('dbs.passwordSaved')" @click="shown = null" />
        </div>
      </template>
    </UModal>
  </template>
</template>
