<script setup lang="ts">
import { repositoryKind } from '~/utils/backupDestination'

// Moving every backup to another repository (issue 192, POST
// /api/backups/repository/move): the panel copies the snapshots, or checks
// the files were already copied. The records follow only once the new place
// holds every one of them. An SFTP destination is reached with the panel's
// own key, shown here (made when there is none) for its authorized_keys.
const open = defineModel<boolean>('open', { default: false })
const props = defineProps<{ current: string }>()

const { t } = useI18n()
const api = useApi()
const { busy, run } = useAction()

const form = reactive({
  mode: 'copy' as 'copy' | 'moved',
  repository: '',
  password: '',
  s3_access_key: '',
  s3_secret_key: '',
  s3_region: '',
  b2_account_id: '',
  b2_account_key: ''
})
watch(open, (v) => {
  if (!v) return
  Object.assign(form, { mode: 'copy', repository: '', password: '', s3_access_key: '', s3_secret_key: '', s3_region: '', b2_account_id: '', b2_account_key: '' })
})
const kind = computed(() => repositoryKind(form.repository))

// The panel's SSH key (GET/POST /api/settings/backup-ssh-key): read when the
// new repository is an SFTP one, and made once when there is none.
const sshKey = ref('')
const sshBusy = ref(false)
async function ensureSSHKey() {
  if (sshKey.value || sshBusy.value) return
  sshBusy.value = true
  try {
    sshKey.value = (await api.get<{ public_key: string }>('/api/settings/backup-ssh-key')).public_key
    if (!sshKey.value) {
      await run(async () => {
        sshKey.value = (await api.post<{ public_key: string }>('/api/settings/backup-ssh-key', {})).public_key
      }, t('ux.backup.sshMade'))
    }
  } catch {
    // The page's own error handling has shown it; the field stays empty.
  } finally {
    sshBusy.value = false
  }
}
watch(kind, (k) => { if (k === 'sftp' && open.value) void ensureSSHKey() })
const modeItems = computed(() => (['copy', 'moved'] as const).map(v => ({
  value: v,
  label: t(`backup.move.mode.${v}`),
  description: t(`backup.move.modeHint.${v}`)
})))
const same = computed(() => form.repository.trim() !== '' && form.repository.trim() === props.current)

async function submit() {
  const body: Record<string, string> = { mode: form.mode, repository: form.repository.trim() }
  for (const k of ['password', 's3_access_key', 's3_secret_key', 's3_region', 'b2_account_id', 'b2_account_key'] as const) {
    if (form[k]) body[k] = form[k]
  }
  if (await run(() => api.post('/api/backups/repository/move', body), t('backup.move.started'))) open.value = false
}
</script>

<template>
  <UModal v-model:open="open" :title="t('backup.move.title')" :description="t('backup.move.hint')">
    <template #body>
      <div class="flex flex-col gap-4">
        <UFormField :label="t('backup.move.from')">
          <p class="mono text-[13px] break-all text-muted">
            {{ current }}
          </p>
        </UFormField>
        <UFormField :label="t('backup.move.to')" :help="t('backup.move.toHelp')" :error="same ? t('backup.move.same') : undefined" required>
          <UInput v-model="form.repository" class="w-full font-mono" placeholder="/mnt/backups/cloudground" autocomplete="off" />
        </UFormField>
        <URadioGroup v-model="form.mode" :items="modeItems" :legend="t('backup.move.how')" />
        <UFormField :label="t('settings.repoPassword')" :hint="t('common.optional')" :help="t('backup.move.passwordHelp')">
          <UInput v-model="form.password" type="password" autocomplete="off" class="w-full font-mono" />
        </UFormField>
        <template v-if="kind === 's3'">
          <UFormField :label="t('ux.backup.accessKey')" :hint="t('common.optional')" :help="t('backup.move.keysHelp')">
            <UInput v-model="form.s3_access_key" autocomplete="off" class="w-full font-mono" />
          </UFormField>
          <UFormField :label="t('ux.backup.secretKey')" :hint="t('common.optional')">
            <UInput v-model="form.s3_secret_key" type="password" autocomplete="off" class="w-full font-mono" />
          </UFormField>
          <UFormField :label="t('settings.region')" :hint="t('common.optional')">
            <UInput v-model="form.s3_region" autocomplete="off" class="w-full font-mono" />
          </UFormField>
        </template>
        <div v-if="kind === 'sftp'" class="flex flex-col gap-2 rounded-md border border-default p-3">
          <p class="text-sm font-medium text-highlighted">
            {{ t('ux.backup.sshTitle') }}
          </p>
          <p class="text-xs text-muted">
            {{ t('ux.backup.sshHint') }}
          </p>
          <USkeleton v-if="sshBusy && !sshKey" class="h-5" />
          <div v-else-if="sshKey" class="flex min-w-0 items-center gap-2">
            <code class="mono min-w-0 flex-1 text-xs [overflow-wrap:anywhere] text-highlighted">{{ sshKey }}</code>
            <CopyButton :value="sshKey" />
          </div>
        </div>
        <template v-if="kind === 'b2'">
          <UFormField :label="t('ux.backup.b2Id')" :hint="t('common.optional')" :help="t('backup.move.keysHelp')">
            <UInput v-model="form.b2_account_id" type="password" autocomplete="off" class="w-full font-mono" />
          </UFormField>
          <UFormField :label="t('ux.backup.b2Key')" :hint="t('common.optional')">
            <UInput v-model="form.b2_account_key" type="password" autocomplete="off" class="w-full font-mono" />
          </UFormField>
        </template>
        <UAlert color="neutral" variant="subtle" icon="i-cg-info" :description="t('backup.move.safe')" />
      </div>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="open = false" />
        <UButton :label="t('backup.move.start')" :loading="busy" :disabled="!form.repository.trim() || same" @click="submit" />
      </div>
    </template>
  </UModal>
</template>
