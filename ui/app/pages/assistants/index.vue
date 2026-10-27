<script setup lang="ts">
/**
 * AI assistants (api SPEC §16): the ones connected to this account (an
 * administrator sees every account's) and the destructive actions they
 * asked for, which wait here for the person to confirm them.
 */
const { t, locale } = useI18n()
const api = useApi()
const route = useRoute()
const proof = useProof()
const confirm = useConfirm()
const { run } = useAction()
const { isAdmin } = useAuth()
useHead({ title: () => t('assistants.title') })

interface Grant { id: number, client_name: string, email: string, scope: string, created_at: string, last_used_at?: string, expires_at: string }
interface Action { id: number, tool: string, summary: string, client_name: string, status: string, created_at: string, expires_at: string, result?: string }

const { data: grants, error: grantsError, refresh: refreshGrants } = useLoad(() => api.get<Grant[]>('/api/mcp/grants'))
const { data: actions, refresh: refreshActions } = useLoad(() => api.get<Action[]>('/api/mcp/actions'))
const highlighted = computed(() => Number(route.query.action ?? 0))
const statusColor = { pending: 'warning', done: 'success', failed: 'error', rejected: 'neutral', expired: 'neutral' } as const

async function revoke(g: Grant) {
  if (await confirm({ title: t('assistants.revokeTitle', { name: g.client_name }), description: t('assistants.revokeHint'), danger: true, confirmLabel: t('security.revoke') })) {
    await run(() => api.del(`/api/mcp/grants/${g.id}`), t('assistants.revoked'))
    await refreshGrants()
  }
}

async function confirmAction(a: Action) {
  const p = await proof({ title: t('assistants.confirmTitle'), description: a.summary, danger: true, confirmLabel: t('assistants.confirmRun') })
  if (!p) return
  await run(() => api.post(`/api/mcp/actions/${a.id}/confirm`, p), t('assistants.confirmed'))
  await refreshActions()
}

async function reject(a: Action) {
  await run(() => api.post(`/api/mcp/actions/${a.id}/reject`, {}), t('assistants.rejected'))
  await refreshActions()
}
</script>

<template>
  <UDashboardPanel id="assistants">
    <template #header>
      <PageNavbar :title="t('assistants.title')" />
    </template>
    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('assistants.title')" :description="t('assistants.hint')" />
        <UAlert v-if="grantsError" color="info" variant="subtle" icon="i-cg-info" :title="t('assistants.off')" :description="isAdmin ? t('assistants.offAdmin') : t('assistants.offOther')" />
        <template v-else>
          <PanelSection id="actions" :title="t('assistants.actions')" :description="t('assistants.actionsHint')" flush>
            <p v-if="actions && !actions.length" class="px-4 py-3 text-sm text-muted sm:px-5">
              {{ t('assistants.actionsNone') }}
            </p>
            <ul v-else class="m-0 list-none divide-y divide-default p-0">
              <li v-for="a in actions ?? []" :key="a.id" class="flex flex-wrap items-center gap-3 px-4 py-3 sm:px-5" :class="a.id === highlighted ? 'bg-elevated' : ''">
                <div class="min-w-0 flex-1">
                  <p class="flex flex-wrap items-center gap-2 text-sm text-highlighted">
                    {{ a.summary }}
                    <UBadge size="sm" variant="subtle" :color="statusColor[a.status as keyof typeof statusColor] ?? 'neutral'" :label="t(`assistants.status_${a.status}`)" />
                  </p>
                  <p class="text-xs text-muted">
                    {{ a.client_name }} · {{ fmtAgo(a.created_at, locale) }}<template v-if="a.status === 'pending'">
                      · {{ t('assistants.expires', { when: fmtAgo(a.expires_at, locale) }) }}
                    </template>
                  </p>
                </div>
                <template v-if="a.status === 'pending'">
                  <UButton size="sm" color="neutral" variant="outline" :label="t('assistants.reject')" @click="reject(a)" />
                  <UButton size="sm" color="error" :label="t('assistants.confirmRun')" @click="confirmAction(a)" />
                </template>
              </li>
            </ul>
          </PanelSection>

          <PanelSection id="connected" :title="t('assistants.connected')" :description="t('assistants.connectedHint')" flush>
            <p v-if="grants && !grants.length" class="px-4 py-3 text-sm text-muted sm:px-5">
              {{ t('assistants.none') }}
            </p>
            <ul v-else class="m-0 list-none divide-y divide-default p-0">
              <li v-for="g in grants ?? []" :key="g.id" class="flex items-center gap-3 px-4 py-3 sm:px-5">
                <UIcon name="i-cg-plug" class="size-5 shrink-0 text-muted" />
                <div class="min-w-0 flex-1">
                  <p class="text-sm text-highlighted">
                    {{ g.client_name }}
                  </p>
                  <p class="mono text-xs [overflow-wrap:anywhere] text-muted">
                    {{ g.email }} · {{ g.scope }} · {{ t('security.tokenMade', { when: fmtAgo(g.created_at, locale) }) }} · {{ g.last_used_at ? t('security.lastSeen', { when: fmtAgo(g.last_used_at, locale) }) : t('security.tokenUnused') }}
                  </p>
                </div>
                <UButton size="sm" color="error" variant="outline" :label="t('security.revoke')" @click="revoke(g)" />
              </li>
            </ul>
          </PanelSection>
        </template>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
