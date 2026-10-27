<script setup lang="ts">
/**
 * A function's releases, newest first, the live one marked: each deploy is
 * one, with its own dependencies, and a rollback makes a chosen one live
 * again (POST /api/functions/{name}/rollback {release}).
 */
const { t, locale } = useI18n()
const api = useApi()
const { isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { fn, base, refresh } = useCurrentFunction()

const rows = computed(() => (fn.value.releases ?? []).map(id => ({ id, time: releaseTime(id), live: id === fn.value.release })))
const when = (r: { id: string, time?: string }) => r.time ? fmtDateTime(r.time, locale.value) : r.id

async function rollback(r: { id: string, time?: string }) {
  const ok = await confirm({ title: t('fn.releases.rollbackTitle', { when: when(r) }), description: t('fn.releases.rollbackHint'), confirmLabel: t('fn.releases.rollback') })
  if (ok && await run(() => api.post(`${base.value}/rollback`, { release: r.id }), t('fn.releases.rolledBack'))) await refresh()
}
</script>

<template>
  <PanelSection :title="t('fn.releases.title')" :description="t('fn.releases.hint')" flush>
    <UEmpty v-if="!rows.length" icon="i-cg-release" :title="t('fn.releases.none')" />
    <ul v-else class="m-0 list-none p-0">
      <li v-for="r in rows" :key="r.id" class="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-default px-4 py-3 last:border-b-0 sm:px-5" :class="r.live ? 'bg-cg-ok-tint' : ''">
        <UIcon name="i-cg-release" class="size-[18px] shrink-0" :class="r.live ? 'text-cg-ok' : 'text-muted'" aria-hidden="true" />
        <span class="flex min-w-0 flex-1 flex-col">
          <span class="text-sm font-medium text-highlighted">{{ when(r) }}<template v-if="r.time"> · <span class="font-normal text-muted">{{ fmtAgo(r.time, locale) }}</span></template></span>
          <span class="mono text-xs break-all text-muted">{{ r.id }}</span>
        </span>
        <UBadge v-if="r.live" color="success" variant="subtle" icon="i-cg-check-circle" :label="t('fn.releases.live')" />
        <UButton v-else-if="isAdmin" size="sm" color="neutral" variant="outline" icon="i-cg-rollback" :label="t('fn.releases.rollback')" :disabled="busy" @click="rollback(r)" />
      </li>
    </ul>
  </PanelSection>
</template>
