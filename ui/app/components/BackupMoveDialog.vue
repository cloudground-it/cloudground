<script setup lang="ts">
// Moving every backup to another repository (issue 192, POST
// /api/backups/repository/move): the panel copies the snapshots, or checks
// the files were already copied. The records follow only once the new place
// holds every one of them.
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
const kind = computed(() => {
  const r = form.repository.trim()
  if (r.startsWith('s3:')) return 's3'
  if (r.startsWith('b2:')) return 'b2'
  return ''
})
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
