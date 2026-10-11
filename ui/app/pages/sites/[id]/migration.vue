<script setup lang="ts">
const { t } = useI18n()
const api = useApi()
const { canManage, isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site } = useCurrentSite()

const form = reactive({ archive: 'shared/migration/site.tar.gz', sql: site.value.config.database.enabled ? 'shared/migration/database.sql' : '', old_domain: '', max_ratio: undefined as number | undefined })

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
</template>
