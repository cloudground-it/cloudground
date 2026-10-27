<script setup lang="ts">
/**
 * External MariaDB and MySQL servers (#30): the servers a site's databases
 * can be put on besides the one on this machine. An administrator adds,
 * changes, tests and removes them; the password is stored sealed and never
 * shown again (GET/POST /api/db-servers, PUT/DELETE …/{id}, POST …/{id}/test;
 * internal/api SPEC §6.5.1).
 */
interface DBServer {
  id: number
  name: string
  kind: string
  host: string
  port: number
  tls: 'required' | 'off'
  ca_cert: string
  admin_user: string
  password_set: boolean
  databases: number
}

const { t } = useI18n()
const api = useApi()
const { isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const toast = useToast()

const { data, refresh } = useLoad(() => api.get<DBServer[]>('/api/db-servers'))
const servers = computed(() => data.value ?? [])

const NAME_RE = /^[a-z][a-z0-9-]{1,31}$/
const open = ref(false)
const editing = ref<DBServer | null>(null)
const form = reactive({ name: '', host: '', port: 3306, tls: 'required' as 'required' | 'off', ca_cert: '', admin_user: '', admin_password: '' })
const tlsItems = computed(() => [
  { value: 'required', label: t('dbservers.tlsRequired') },
  { value: 'off', label: t('dbservers.tlsOff') }
])

function start(s?: DBServer) {
  editing.value = s ?? null
  Object.assign(form, s
    ? { name: s.name, host: s.host, port: s.port, tls: s.tls, ca_cert: s.ca_cert, admin_user: s.admin_user, admin_password: '' }
    : { name: '', host: '', port: 3306, tls: 'required', ca_cert: '', admin_user: '', admin_password: '' })
  open.value = true
}
const valid = computed(() => NAME_RE.test(form.name) && form.host.trim() !== '' && form.admin_user.trim() !== ''
  && (editing.value !== null || form.admin_password !== '') && Number.isInteger(form.port) && form.port >= 1 && form.port <= 65535)

async function save() {
  const body: Record<string, unknown> = { name: form.name, host: form.host.trim(), port: form.port, tls: form.tls, ca_cert: form.ca_cert, admin_user: form.admin_user.trim() }
  if (form.admin_password) body.admin_password = form.admin_password
  const e = editing.value
  const ok = await run(() => e ? api.put(`/api/db-servers/${e.id}`, body) : api.post('/api/db-servers', body), t('dbservers.saved'))
  if (ok) {
    open.value = false
    await refresh()
  }
}
async function test(s: DBServer) {
  await run(async () => {
    const r = await api.post<{ version: string }>(`/api/db-servers/${s.id}/test`)
    toast.add({ title: t('dbservers.connected', { version: r.version }), color: 'success', icon: 'i-cg-check-circle' })
  })
}
async function remove(s: DBServer) {
  const ok = await confirm({ title: t('dbservers.removeTitle', { name: s.name }), description: t('dbservers.removeHint'), danger: true, confirmLabel: t('common.delete') })
  if (ok && await run(() => api.del(`/api/db-servers/${s.id}`), t('dbservers.removed'))) await refresh()
}
</script>

<template>
  <PanelSection id="dbservers" :title="t('dbservers.title')" :description="t('dbservers.hint')" icon="i-cg-database">
    <template v-if="isAdmin" #actions>
      <UButton icon="i-cg-plus" :label="t('dbservers.add')" @click="start()" />
    </template>
    <p v-if="!servers.length" class="m-0 text-sm text-muted">
      {{ t('dbservers.none') }}
    </p>
    <ul v-else class="m-0 list-none divide-y divide-default border border-default p-0">
      <li v-for="s in servers" :key="s.id" class="flex flex-wrap items-center gap-x-3 gap-y-2 px-3 py-2.5">
        <span class="mono text-[13px] font-semibold text-highlighted">{{ s.name }}</span>
        <span class="mono text-xs text-muted">{{ s.host }}:{{ s.port }}</span>
        <UBadge :color="s.tls === 'required' ? 'success' : 'warning'" variant="subtle" :label="s.tls === 'required' ? t('dbservers.tlsRequired') : t('dbservers.tlsOff')" />
        <span class="text-xs text-muted">{{ t('dbservers.databases', s.databases) }}</span>
        <span v-if="isAdmin" class="ms-auto flex items-center gap-1">
          <UButton size="sm" color="neutral" variant="outline" :label="t('dbservers.test')" :disabled="busy" @click="test(s)" />
          <UButton size="sm" color="neutral" variant="outline" :label="t('dbservers.edit')" :disabled="busy" @click="start(s)" />
          <UButton size="sm" color="neutral" variant="ghost" square icon="i-cg-trash" :aria-label="t('common.delete')" :disabled="busy || s.databases > 0" :title="s.databases > 0 ? t('dbservers.inUse') : undefined" @click="remove(s)" />
        </span>
      </li>
    </ul>

    <UModal v-model:open="open" :title="editing ? t('dbservers.editTitle', { name: editing.name }) : t('dbservers.add')" :description="t('dbservers.hint')">
      <template #body>
        <UForm id="db-server" :state="form" class="flex flex-col gap-4" @submit="save">
          <UFormField :label="t('dbservers.name')" :help="t('dbservers.nameHelp')" :error="form.name && !NAME_RE.test(form.name) ? t('dbservers.nameHelp') : undefined" name="name">
            <UInput v-model="form.name" class="w-full font-mono" placeholder="managed" autocomplete="off" spellcheck="false" />
          </UFormField>
          <div class="grid grid-cols-3 gap-4">
            <UFormField :label="t('dbservers.host')" class="col-span-2" name="host">
              <UInput v-model="form.host" class="w-full font-mono" placeholder="db.example.com" autocomplete="off" spellcheck="false" />
            </UFormField>
            <UFormField :label="t('mail.port')" name="port">
              <UInputNumber v-model="form.port" :min="1" :max="65535" class="w-full" />
            </UFormField>
          </div>
          <UFormField :label="t('dbservers.tls')" :help="form.tls === 'off' ? t('dbservers.tlsOffWarn') : undefined" name="tls">
            <USelect v-model="form.tls" :items="tlsItems" class="w-full sm:w-64" />
          </UFormField>
          <UFormField v-if="form.tls === 'required'" :label="t('dbservers.ca')" :help="t('dbservers.caHelp')" name="ca_cert">
            <UTextarea v-model="form.ca_cert" :rows="4" class="w-full font-mono text-xs" placeholder="-----BEGIN CERTIFICATE-----" />
          </UFormField>
          <UFormField :label="t('dbservers.adminUser')" :help="t('dbservers.adminHelp')" name="admin_user">
            <UInput v-model="form.admin_user" class="w-full font-mono" autocomplete="off" spellcheck="false" />
          </UFormField>
          <UFormField :label="t('dbservers.adminPassword')" :help="editing ? t('dbservers.passwordKept') : undefined" name="admin_password">
            <UInput v-model="form.admin_password" type="password" autocomplete="new-password" class="w-full font-mono" />
          </UFormField>
        </UForm>
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="open = false" />
          <UButton color="primary" variant="solid" type="submit" form="db-server" :label="t('common.save')" :disabled="!valid" :loading="busy" />
        </div>
      </template>
    </UModal>
  </PanelSection>
</template>
