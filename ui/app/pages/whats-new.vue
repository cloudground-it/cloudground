<script setup lang="ts">
/**
 * What changed in each release, newest first, from the CHANGELOG.md inside
 * the panel (GET /api/changelog, issue 106). The entries are in English; the
 * rest of the page is translated.
 */
const { t } = useI18n()
const { log, load, markSeen } = useChangelog()
useHead({ title: () => t('whatsnew.title') })

const fullURL = 'https://github.com/cloudground-it/cloudground/blob/main/CHANGELOG.md'
const { pending } = useLoad(async () => {
  const l = await load()
  // Having read the list is having seen it.
  if (l?.unseen.length) await markSeen().catch(() => {})
  return l
})
const wasNew = ref<string[]>([])
watch(log, (l) => {
  if (l?.unseen.length) wasNew.value = [...l.unseen]
}, { immediate: true })
</script>

<template>
  <UDashboardPanel id="whats-new">
    <template #header>
      <PageNavbar :title="t('whatsnew.title')" />
    </template>
    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('whatsnew.title')" :description="t('whatsnew.hint')">
          <template #links>
            <UButton color="neutral" variant="outline" icon="i-cg-external-link" :label="t('whatsnew.full')" :to="fullURL" target="_blank" rel="noopener" />
          </template>
        </UPageHeader>
        <USkeleton v-if="pending && !log" class="h-64" />
        <UEmpty v-else-if="!log?.entries.length" icon="i-cg-history" :title="t('whatsnew.none')" />
        <PanelSection v-for="e in log?.entries ?? []" :key="e.version" :title="e.version">
          <template #actions>
            <UBadge v-if="wasNew.includes(e.version)" color="primary" variant="subtle" :label="t('whatsnew.new')" />
            <UBadge v-else-if="e.version === log?.current" color="neutral" variant="outline" :label="t('whatsnew.running')" />
          </template>
          <MarkdownText :source="e.body" />
        </PanelSection>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
