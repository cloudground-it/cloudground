<script setup lang="ts">
import type { Alert } from '~/types'

defineOptions({ inheritAttrs: false })
const { t } = useI18n()
const api = useApi()
const alerts = useState<Alert[]>('alerts:list', () => [])
const open = useState<boolean>('alerts:open', () => false)

useLive(async () => {
  alerts.value = await api.get<Alert[]>('/api/system/alerts')
  return alerts.value
}, { on: { types: ['alert.'] }, fallbackMs: 60000 })
</script>

<template>
  <UButton
    v-bind="$attrs"
    color="neutral"
    variant="ghost"
    square
    class="relative"
    :aria-label="alerts.length ? t('cg.shell.alertsCount', { n: alerts.length }) : t('alerts.title')"
    @click="open = true"
  >
    <UIcon name="i-cg-bell" class="size-5" />
    <span v-if="alerts.length" class="mono absolute top-0.5 right-0.5 min-w-4 bg-cg-primary px-1 text-center text-[10px] leading-4 font-medium text-cg-on-primary" aria-hidden="true">{{ alerts.length }}</span>
  </UButton>
  <USlideover v-model:open="open" :title="t('alerts.title')" :description="t('alerts.description')">
    <template #body>
      <AlertList :alerts="alerts" @dismissed="id => alerts = alerts.filter(a => a.id !== id)" />
    </template>
  </USlideover>
</template>
