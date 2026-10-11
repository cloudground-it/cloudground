<script setup lang="ts">
const { t } = useI18n()
useHead({ title: () => t('nav.logs') })
const source = ref('nginx-error')
const items = [
  { label: 'Nginx access', value: 'nginx-access', icon: 'i-cg-activity' },
  { label: 'Nginx error', value: 'nginx-error', icon: 'i-cg-alert-triangle' },
  { label: 'PHP-FPM', value: 'php-error', icon: 'i-cg-code' },
  { label: 'MariaDB', value: 'mariadb', icon: 'i-cg-database' },
  { label: 'API', value: 'api', icon: 'i-cg-server' },
  { label: 'Agent', value: 'agent', icon: 'i-cg-cpu' }
]
</script>

<template>
  <UDashboardPanel id="logs">
    <template #header>
      <PageNavbar :title="t('nav.logs')" />
    </template>
    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('nav.logs')" :description="t('logs.hint')" />
        <div class="flex flex-col gap-4">
          <div class="scrollbar-none overflow-x-auto overflow-y-hidden">
            <UTabs v-model="source" :items="items" :content="false" class="w-max min-w-full" />
          </div>
          <LogViewer :source="source" />
        </div>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
