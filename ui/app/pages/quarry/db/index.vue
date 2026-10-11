<script setup lang="ts">
/* No database chosen: the session's first, or a word that there is none. */
definePageMeta({ layout: 'dbstudio', middleware: 'studio' })

const { t } = useI18n()
const { session, dbLink } = useStudio()
const { databases, loadDatabases } = useStudioSchema()
useHead({ title: () => t('studio.databases') })
const error = ref<string | null>(null)

onMounted(async () => {
  try {
    const list = await loadDatabases()
    const db = session.value?.database && list.includes(session.value.database) ? session.value.database : list[0]
    if (db) await navigateTo(dbLink(db), { replace: true })
  } catch (e) {
    error.value = errorMessage(e)
  }
})
</script>

<template>
  <div class="flex flex-col gap-4">
    <UAlert v-if="error" color="error" variant="subtle" :description="error" />
    <UEmpty v-else-if="databases && !databases.length" icon="i-cg-database" :title="t('studio.noDatabases')" :description="t('studio.noDatabasesHint')" />
    <USkeleton v-else class="h-24" />
  </div>
</template>
