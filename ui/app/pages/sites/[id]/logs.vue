<script setup lang="ts">
const { t } = useI18n()
const { site } = useCurrentSite()
const source = ref('access')
const items = computed(() => [
  { label: t('logs.access'), value: 'access', icon: 'i-cg-activity' },
  { label: t('logs.error'), value: 'error', icon: 'i-cg-alert-triangle' },
  { label: t('logs.php'), value: 'php', icon: 'i-cg-code' }
])
</script>

<template>
  <div class="flex flex-col gap-4">
    <div class="scrollbar-none overflow-x-auto overflow-y-hidden">
      <UTabs v-model="source" :items="items" :content="false" class="w-max min-w-full" />
    </div>
    <LogViewer :source="source" :site-id="site.id" :site="site.domain" :lines="200" />
  </div>
</template>
