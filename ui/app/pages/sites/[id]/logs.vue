<script setup lang="ts">
const { t } = useI18n()
const { site } = useCurrentSite()
const route = useRoute()
// A Node.js or Python site's app writes its output to the app log (#28).
const source = ref(route.query.source === 'app' && isAppSite(site.value) ? 'app' : 'access')
const items = computed(() => [
  ...(isAppSite(site.value) ? [{ label: t('logs.app'), value: 'app', icon: 'i-cg-terminal' }] : []),
  { label: t('logs.access'), value: 'access', icon: 'i-cg-activity' },
  { label: t('logs.error'), value: 'error', icon: 'i-cg-alert-triangle' },
  ...(isAppSite(site.value) ? [] : [{ label: t('logs.php'), value: 'php', icon: 'i-cg-code' }]),
  // The weekly database cleanup writes its results here (WordPress sites).
  ...(site.value.type === 'wordpress' || site.value.type === 'woocommerce' ? [{ label: t('logs.cleanup'), value: 'cleanup', icon: 'i-cg-purge' }] : [])
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
