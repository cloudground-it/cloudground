<script setup lang="ts">
const { t } = useI18n()
const api = useApi()
const { canManage, isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site } = useCurrentSite()

const form = reactive({ archive: 'shared/migration/site.tar.gz', sql: site.value.config.database.enabled ? 'shared/migration/database.sql' : '', old_domain: '', max_ratio: undefined as number | undefined })

// Pulling the old site from its host over SSH/SFTP (issue 82): a dry run
// first (size, WordPress or not, the host's key), then the pull, with the
// host's key from the dry run pinned. The password or key is sent once and
// kept nowhere.
const pull = reactive({ host: '', port: 22, user: '', auth: 'password' as 'password' | 'key', password: '', private_key: '', path: '', db: '' as '' | 'dump' | 'file', sql_path: '' })
interface Estimate { files: number, bytes: number, wordpress: boolean, shell: boolean, host_key: string, free_bytes: number, fits: boolean }
const estimate = ref<Estimate | null>(null)
const estimating = ref(false)
const pullError = ref<string | null>(null)
const authItems = computed(() => [{ label: t('migration.pullPassword'), value: 'password' }, { label: t('migration.pullKey'), value: 'key' }])
const dbItems = computed(() => [
  { label: t('migration.pullDbNone'), value: '' },
  { label: t('migration.pullDbDump'), value: 'dump' },
  { label: t('migration.pullDbFile'), value: 'file' }
])
function pullBody(): Record<string, unknown> {
  return {
    host: pull.host.trim(), port: pull.port || 22, user: pull.user.trim(), path: pull.path.trim(),
    ...(pull.auth === 'password' ? { password: pull.password } : { private_key: pull.private_key }),
    db: pull.db, sql_path: pull.db === 'file' ? pull.sql_path.trim() : undefined,
    old_domain: form.old_domain || undefined
  }
}
watch(() => [pull.host, pull.port, pull.user, pull.path, pull.db], () => { estimate.value = null })
const pullReady = computed(() => pull.host.trim() && pull.user.trim() && pull.path.trim() && (pull.auth === 'password' ? pull.password : pull.private_key) && (pull.db !== 'file' || pull.sql_path.trim()))
async function measure() {
  estimating.value = true
  pullError.value = null
  try {
    estimate.value = await api.post<Estimate>(`/api/sites/${site.value.id}/migration/pull/estimate`, pullBody())
  } catch (e) {
    pullError.value = errorMessage(e)
  } finally {
    estimating.value = false
  }
}
async function startPull() {
  if (!estimate.value) return
  const ok = await confirm({ title: t('migration.confirmTitle'), description: pull.db ? t('migration.confirmBoth') : t('migration.confirmFiles'), danger: true, typed: site.value.domain, confirmLabel: t('migration.pullStart') })
  if (!ok) return
  if (await run(() => api.post(`/api/sites/${site.value.id}/migration/pull`, { ...pullBody(), host_key: estimate.value!.host_key, confirm: site.value.domain }), t('migration.started'))) {
    pull.password = ''
    pull.private_key = ''
    estimate.value = null
  }
}

async function submit() {
  const ok = await confirm({ title: t('migration.confirmTitle'), description: form.sql ? t('migration.confirmBoth') : t('migration.confirmFiles'), danger: true, typed: site.value.domain, confirmLabel: t('migration.import') })
  if (ok) {
    await run(() => api.post(`/api/sites/${site.value.id}/migration`, {
      archive: form.archive, sql: form.sql || undefined, old_domain: form.old_domain || undefined, confirm: site.value.domain,
      // The API takes the expansion limit from administrators only.
      max_ratio: isAdmin.value && form.max_ratio ? form.max_ratio : undefined
    }), t('migration.started'))
  }
}
</script>

<template>
  <PanelSection :title="t('migration.title')" :description="t('migration.hint')">
    <UForm id="migration-form" :state="form" class="flex max-w-2xl flex-col gap-4" @submit="submit">
      <UFormField :label="t('migration.archive')" :help="t('migration.archiveHint')" required>
        <UInput v-model="form.archive" class="w-full font-mono" />
      </UFormField>
      <UFormField v-if="site.config.database.enabled" :label="t('migration.sql')" :hint="t('common.optional')">
        <UInput v-model="form.sql" class="w-full font-mono" />
      </UFormField>
      <UFormField v-if="isWordPress(site)" :label="t('migration.oldDomain')" :help="t('migration.oldDomainHint')">
        <UInput v-model="form.old_domain" class="w-full" placeholder="old.example.com" />
      </UFormField>
      <UFormField v-if="isAdmin" :label="t('migration.expansionLimit')" :help="t('migration.expansionLimitHint')">
        <UInputNumber v-model="form.max_ratio" :min="1" :max="1000" placeholder="200" class="w-40" />
      </UFormField>
    </UForm>
    <template #footer>
      <UButton type="submit" form="migration-form" icon="i-cg-import" :label="t('migration.import')" :disabled="!canManage" :loading="busy" />
    </template>
  </PanelSection>

  <PanelSection v-if="isAdmin" :title="t('migration.pullTitle')" :description="t('migration.pullHint')">
    <div class="flex max-w-2xl flex-col gap-4">
      <UAlert v-if="pullError" color="error" variant="subtle" icon="i-cg-alert-circle" :title="pullError" />
      <div class="grid gap-4 sm:grid-cols-[1fr_7rem]">
        <UFormField :label="t('migration.pullHost')" required>
          <UInput v-model="pull.host" class="w-full font-mono" placeholder="old-server.example.com" autocomplete="off" />
        </UFormField>
        <UFormField :label="t('migration.pullPort')">
          <UInputNumber v-model="pull.port" :min="1" :max="65535" class="w-full" />
        </UFormField>
      </div>
      <UFormField :label="t('migration.pullUser')" required>
        <UInput v-model="pull.user" class="w-full font-mono" autocomplete="off" />
      </UFormField>
      <URadioGroup v-model="pull.auth" :items="authItems" orientation="horizontal" />
      <UFormField v-if="pull.auth === 'password'" :label="t('common.password')" :help="t('migration.pullOnce')">
        <UInput v-model="pull.password" type="password" class="w-full" autocomplete="off" />
      </UFormField>
      <UFormField v-else :label="t('migration.pullKey')" :help="t('migration.pullOnce')">
        <UTextarea v-model="pull.private_key" :rows="4" class="w-full font-mono" autocomplete="off" placeholder="-----BEGIN OPENSSH PRIVATE KEY-----" />
      </UFormField>
      <UFormField :label="t('migration.pullPath')" :help="t('migration.pullPathHint')" required>
        <UInput v-model="pull.path" class="w-full font-mono" placeholder="/var/www/html" autocomplete="off" />
      </UFormField>
      <UFormField :label="t('migration.pullDb')">
        <URadioGroup v-model="pull.db" :items="dbItems" />
      </UFormField>
      <UFormField v-if="pull.db === 'file'" :label="t('migration.sql')" required>
        <UInput v-model="pull.sql_path" class="w-full font-mono" placeholder="/home/user/database.sql" autocomplete="off" />
      </UFormField>
      <UAlert v-if="estimate" :color="estimate.fits ? 'info' : 'warning'" variant="subtle" icon="i-cg-info"
        :title="t('migration.pullEstimate', { files: fmtNumber(estimate.files), size: fmtBytes(estimate.bytes) })">
        <template #description>
          <p>{{ estimate.wordpress ? t('migration.pullIsWordPress') : t('migration.pullNotWordPress') }}</p>
          <p v-if="pull.db === 'dump' && !estimate.shell">
            {{ t('migration.pullNoShell') }}
          </p>
          <p v-if="!estimate.fits">
            {{ t('migration.pullNoRoom', { free: fmtBytes(estimate.free_bytes) }) }}
          </p>
          <p class="mono text-xs [overflow-wrap:anywhere]">
            {{ t('migration.pullHostKey', { key: estimate.host_key }) }}
          </p>
        </template>
      </UAlert>
    </div>
    <template #footer>
      <div class="flex flex-wrap gap-2">
        <UButton color="neutral" variant="outline" icon="i-cg-search" :label="t('migration.pullMeasure')" :loading="estimating" :disabled="!pullReady" @click="measure" />
        <UButton icon="i-cg-import" :label="t('migration.pullStart')" :loading="busy" :disabled="!pullReady || !estimate || !estimate.fits || (pull.db === 'dump' && !estimate.shell)" @click="startPull" />
      </div>
    </template>
  </PanelSection>
</template>
