<script setup lang="ts">
/**
 * The server's block list (#21): addresses blocked for repeated failed
 * sign-ins at SSH, WordPress and the panel, or by hand; the addresses never
 * blocked; and the automatic blocking's policy. Each site's own refused
 * visitors (#25) sit beside it (SiteBlockingRules). Netfilter drops a blocked
 * address on every port (GET/POST/DELETE /api/blocklist…).
 */
interface Entry { id: number, address: string, source: 'manual' | 'ssh' | 'wordpress' | 'panel', note: string, failures?: number, created_at: string, expires_at?: string }
interface Settings { enabled: boolean, max_failures: number, window_minutes: number, ban_minutes: number }
interface Blocklist {
  settings: Settings
  blocked: Entry[]
  allowed: Entry[]
  enforced: boolean
  error?: string
  applied_at?: string
  watching: { ssh: boolean, wordpress: boolean }
  your_address: string
}

const { t, locale } = useI18n()
const api = useApi()
const { isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
useHead({ title: () => t('nav.blocklist') })

const list = ref<Blocklist | null>(null)
const { data, refresh } = useLoad(() => api.get<Blocklist>('/api/blocklist'))
watch(data, (v) => { if (v) list.value = v })
// Every change answers the list as it is now.
async function change(fn: () => Promise<Blocklist>, done: string): Promise<boolean> {
  return run(async () => { list.value = await fn() }, done)
}
// Automatic blocks end on their own: the page follows them.
const timer = setInterval(() => { void refresh() }, 30000)
onBeforeUnmount(() => clearInterval(timer))

// An address or a CIDR, as the server reads it (internal/blocklist §1).
const addressOK = (s: string) => /^[0-9A-Fa-f:.]+(\/\d{1,3})?$/.test(s.trim()) && /[.:]/.test(s)

// Blocking by hand.
const blocking = ref(false)
const blockForm = reactive({ address: '', note: '', minutes: 0 })
const durations = computed(() => [
  { label: t('blocklist.forHour'), value: 60 },
  { label: t('blocklist.forDay'), value: 1440 },
  { label: t('blocklist.forWeek'), value: 10080 },
  { label: t('blocklist.untilRemoved'), value: 0 }
])
function openBlock() {
  Object.assign(blockForm, { address: '', note: '', minutes: 0 })
  blocking.value = true
}
async function block() {
  if (await change(() => api.post<Blocklist>('/api/blocklist/blocks', { address: blockForm.address.trim(), note: blockForm.note, minutes: blockForm.minutes }), t('blocklist.blocked', { address: blockForm.address.trim() }))) {
    blocking.value = false
  }
}
async function unblock(e: Entry) {
  if (await confirm({ title: t('blocklist.unblockTitle', { address: e.address }), description: t('blocklist.unblockHint'), confirmLabel: t('blocklist.unblock') })) {
    await change(() => api.del<Blocklist>('/api/blocklist/blocks', {}, { address: e.address }), t('blocklist.unblocked', { address: e.address }))
  }
}

// The allowlist.
const allowForm = reactive({ address: '', note: '' })
const mine = computed(() => list.value?.your_address ?? '')
const mineAllowed = computed(() => !!list.value?.allowed.some(e => e.address === mine.value + '/32' || e.address === mine.value + '/128'))
const mineIsIP = computed(() => addressOK(mine.value))
async function allow(address: string, note: string) {
  if (await change(() => api.post<Blocklist>('/api/blocklist/allowed', { address: address.trim(), note }), t('blocklist.allowedDone', { address: address.trim() }))) {
    Object.assign(allowForm, { address: '', note: '' })
  }
}
async function disallow(e: Entry) {
  if (await confirm({ title: t('blocklist.disallowTitle', { address: e.address }), description: t('blocklist.disallowHint'), danger: true, confirmLabel: t('common.remove') })) {
    await change(() => api.del<Blocklist>('/api/blocklist/allowed', {}, { address: e.address }), t('blocklist.disallowed', { address: e.address }))
  }
}

// The automatic blocking's policy.
const policy = reactive<Settings>({ enabled: true, max_failures: 6, window_minutes: 10, ban_minutes: 60 })
watch(() => list.value?.settings, (s) => { if (s) Object.assign(policy, s) }, { immediate: true })
const policyOK = computed(() => policy.max_failures >= 3 && policy.max_failures <= 100 && policy.window_minutes >= 1 && policy.window_minutes <= 1440 && policy.ban_minutes >= 1 && policy.ban_minutes <= 43200)
async function savePolicy() {
  await change(() => api.put<Blocklist>('/api/blocklist/settings', { ...policy }), t('common.saved'))
}

const sourceColor = (s: Entry['source']) => s === 'manual' ? 'neutral' : 'warning'
const blockedColumns = computed(() => [
  { id: 'address', header: t('blocklist.address') },
  { id: 'source', header: t('blocklist.source') },
  { id: 'since', header: t('blocklist.since') },
  { id: 'until', header: t('blocklist.until') },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])
</script>

<template>
  <UDashboardPanel id="blocklist">
    <template #header>
      <PageNavbar :title="t('nav.blocklist')" />
    </template>
    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('nav.blocklist')" :description="t('ux.pages.blocklist')" />
        <UAlert v-if="list && !list.enforced" color="warning" variant="subtle" icon="i-cg-alert-triangle" :title="t('blocklist.notEnforced')" :description="list.error || t('blocklist.notEnforcedYet')" />

        <PanelSection :title="t('blocklist.blockedTitle')" :description="t('blocklist.blockedHint')" flush>
          <template v-if="isAdmin" #actions>
            <UButton icon="i-cg-plus" :label="t('blocklist.block')" @click="openBlock" />
          </template>
          <USkeleton v-if="!list" class="m-4 h-24" />
          <UEmpty v-else-if="!list.blocked.length" icon="i-cg-shield-check" :description="t('blocklist.noneBlocked')" />
          <UTable v-else :data="list.blocked" :columns="blockedColumns">
            <template #address-cell="{ row: { original: e } }">
              <span class="mono text-[13px] text-highlighted">{{ e.address }}</span>
              <span v-if="e.note" class="block text-[12px] text-muted">{{ e.note }}</span>
            </template>
            <template #source-cell="{ row: { original: e } }">
              <UBadge :color="sourceColor(e.source)" variant="subtle" :label="t(`blocklist.sources.${e.source}`)" />
            </template>
            <template #since-cell="{ row: { original: e } }">
              <span class="text-[13px]" :title="fmtDateTime(e.created_at, locale)">{{ fmtAgo(e.created_at, locale) }}</span>
            </template>
            <template #until-cell="{ row: { original: e } }">
              <span class="text-[13px]">{{ e.expires_at ? fmtDateTime(e.expires_at, locale) : t('blocklist.untilRemoved') }}</span>
            </template>
            <template #actions-cell="{ row: { original: e } }">
              <UButton v-if="isAdmin" size="sm" color="neutral" variant="ghost" icon="i-cg-unlock" :label="t('blocklist.unblock')" @click="unblock(e)" />
            </template>
          </UTable>
        </PanelSection>

        <SiteBlockingRules />

        <div class="grid items-start gap-6 lg:grid-cols-2">
          <PanelSection :title="t('blocklist.allowTitle')" :description="t('blocklist.allowHint')" flush>
            <USkeleton v-if="!list" class="m-4 h-16" />
            <template v-else>
              <p v-if="mineIsIP" class="m-0 flex flex-wrap items-center gap-2 border-b border-default px-4 py-3 text-[13px] text-muted sm:px-5">
                <span>{{ t('blocklist.yourAddress') }} <span class="mono text-highlighted">{{ mine }}</span></span>
                <UBadge v-if="mineAllowed" color="success" variant="subtle" :label="t('blocklist.allowedBadge')" />
                <UButton v-else-if="isAdmin" size="xs" color="neutral" variant="outline" icon="i-cg-shield-check" :label="t('blocklist.allowMine')" :loading="busy" @click="allow(mine, t('blocklist.mineNote'))" />
              </p>
              <UEmpty v-if="!list.allowed.length" size="sm" icon="i-cg-shield-check" :description="t('blocklist.noneAllowed')" />
              <ul v-else class="m-0 list-none divide-y divide-default p-0">
                <li v-for="e in list.allowed" :key="e.id" class="flex items-center gap-3 px-4 py-2.5 sm:px-5">
                  <span class="mono min-w-0 flex-1 truncate text-[13px] text-highlighted">{{ e.address }}<span v-if="e.note" class="ms-2 font-sans text-muted">{{ e.note }}</span></span>
                  <UButton v-if="isAdmin" size="sm" color="error" variant="ghost" square icon="i-cg-trash" :aria-label="t('blocklist.disallowTitle', { address: e.address })" @click="disallow(e)" />
                </li>
              </ul>
            </template>
            <template v-if="isAdmin" #footer>
              <UForm id="allow-form" :state="allowForm" class="flex w-full flex-col gap-3 sm:flex-row sm:items-end" @submit="allow(allowForm.address, allowForm.note)">
                <UFormField :label="t('blocklist.address')" class="flex-1">
                  <UInput v-model="allowForm.address" class="w-full font-mono" placeholder="203.0.113.4" />
                </UFormField>
                <UFormField :label="t('blocklist.note')" :hint="t('common.optional')" class="flex-1">
                  <UInput v-model="allowForm.note" class="w-full" maxlength="200" />
                </UFormField>
                <UButton type="submit" :label="t('blocklist.allow')" :disabled="!addressOK(allowForm.address)" :loading="busy" />
              </UForm>
            </template>
          </PanelSection>

          <PanelSection :title="t('blocklist.autoTitle')" :description="t('blocklist.autoHint')">
            <div class="flex flex-col gap-4">
              <USwitch v-model="policy.enabled" :label="t('blocklist.autoEnabled')" :disabled="!isAdmin" />
              <div class="grid gap-4 sm:grid-cols-3">
                <UFormField :label="t('blocklist.maxFailures')" hint="3–100">
                  <UInputNumber v-model="policy.max_failures" :min="3" :max="100" class="w-full" :disabled="!isAdmin || !policy.enabled" />
                </UFormField>
                <UFormField :label="t('blocklist.window')" hint="1–1440">
                  <UInputNumber v-model="policy.window_minutes" :min="1" :max="1440" class="w-full" :disabled="!isAdmin || !policy.enabled" />
                </UFormField>
                <UFormField :label="t('blocklist.banFor')" hint="1–43200">
                  <UInputNumber v-model="policy.ban_minutes" :min="1" :max="43200" class="w-full" :disabled="!isAdmin || !policy.enabled" />
                </UFormField>
              </div>
              <ul v-if="list" class="m-0 flex list-none flex-wrap gap-2 p-0 text-[13px]">
                <li><UBadge :color="list.watching.ssh ? 'success' : 'warning'" variant="subtle" :label="t(list.watching.ssh ? 'blocklist.watchingSSH' : 'blocklist.notWatchingSSH')" /></li>
                <li><UBadge :color="list.watching.wordpress ? 'success' : 'warning'" variant="subtle" :label="t(list.watching.wordpress ? 'blocklist.watchingWP' : 'blocklist.notWatchingWP')" /></li>
                <li><UBadge color="neutral" variant="subtle" :label="t('blocklist.watchingPanel')" /></li>
              </ul>
            </div>
            <template v-if="isAdmin" #footer>
              <UButton :label="t('common.save')" :disabled="!policyOK" :loading="busy" @click="savePolicy" />
            </template>
          </PanelSection>
        </div>
      </UContainer>

      <UModal v-model:open="blocking" :title="t('blocklist.block')" :description="t('blocklist.blockHint')">
        <template #body>
          <div class="flex flex-col gap-4">
            <UFormField :label="t('blocklist.address')" :help="t('blocklist.addressHelp')" :error="blockForm.address && !addressOK(blockForm.address) ? t('blocklist.addressBad') : undefined">
              <UInput v-model="blockForm.address" class="w-full font-mono" placeholder="203.0.113.0/24" autofocus />
            </UFormField>
            <UFormField :label="t('blocklist.duration')">
              <USelect v-model="blockForm.minutes" :items="durations" class="w-full" />
            </UFormField>
            <UFormField :label="t('blocklist.note')" :hint="t('common.optional')">
              <UInput v-model="blockForm.note" class="w-full" maxlength="200" />
            </UFormField>
          </div>
        </template>
        <template #footer>
          <div class="flex w-full justify-end gap-2">
            <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="blocking = false" />
            <UButton color="primary" variant="solid" icon="i-cg-lock" :label="t('blocklist.block')" :disabled="!addressOK(blockForm.address)" :loading="busy" @click="block" />
          </div>
        </template>
      </UModal>
    </template>
  </UDashboardPanel>
</template>
