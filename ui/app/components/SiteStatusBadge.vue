<script setup lang="ts">
import type { Site } from '~/types'

defineProps<{ status: Site['status'], staging?: boolean }>()
const { t } = useI18n()
const color = { provisioning: 'info', active: 'success', error: 'error', deleting: 'warning' } as const
</script>

<template>
  <UBadge v-if="staging && status === 'active'" color="neutral" variant="subtle" label="staging" />
  <UBadge v-else :color="color[status]" variant="subtle" :label="t(`site.status.${status}`)" />
</template>
