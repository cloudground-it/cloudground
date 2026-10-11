<script setup lang="ts">
const { t } = useI18n()
const api = useApi()
const { isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
useHead({ title: () => t('nav.firewall') })

interface PlanRule { port: number, proto: 'tcp' | 'udp', service: 'ssh' | 'http' | 'https' | 'http3' | 'panel' }
const { data: status, refresh } = useLoad(() => api.get<{ enabled: boolean, rules: string[] | null, plan?: PlanRule[] }>('/api/firewall'))
// ufw installed and off: the rules turning it on allows first, as the
// server computed them (the ports sshd really listens on among them).
const plan = computed(() => status.value && !status.value.enabled ? status.value.plan ?? [] : [])
const ruleText = (r: PlanRule) => `${r.port}/${r.proto}`

async function enable() {
  const ok = await confirm({
    title: t('firewall.enableTitle'),
    description: t('firewall.enableConfirm', { rules: plan.value.map(ruleText).join(', ') }),
    confirmLabel: t('firewall.enable')
  })
  if (!ok) return
  // The server refuses when its rules are no longer the ones shown here.
  if (await run(() => api.post('/api/firewall/enable', { expect: plan.value.map(ruleText) }), t('firewall.enabled'))) {
    await refresh()
  }
}
const form = reactive({ action: 'allow', port: undefined as number | undefined, from: '' })
const actions = computed(() => [
  { label: t('firewall.allow'), value: 'allow' },
  { label: t('firewall.deny'), value: 'deny' },
  { label: t('firewall.delete'), value: 'delete' }
])

async function apply() {
  if (await run(() => api.post('/api/firewall/rule', { action: form.action, port: Number(form.port), proto: 'tcp', from: form.from || undefined }), t('firewall.applied'))) {
    form.port = undefined
    form.from = ''
    await refresh()
  }
}
</script>

<template>
  <UDashboardPanel id="firewall">
    <template #header>
      <PageNavbar :title="t('nav.firewall')" />
    </template>
    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('nav.firewall')" :description="t('ux.pages.firewall')" />
        <PanelSection v-if="isAdmin && plan.length" :title="t('firewall.enableTitle')" :description="t('firewall.enableHint')" flush>
          <ul class="m-0 list-none divide-y divide-default p-0">
            <li v-for="r in plan" :key="ruleText(r)" class="flex items-center gap-4 px-4 py-2.5 sm:px-5">
              <UIcon name="i-cg-check" class="size-4 shrink-0 text-cg-ok" />
              <span class="mono w-20 shrink-0 text-[13px] text-highlighted">{{ ruleText(r) }}</span>
              <span class="min-w-0 text-[13px] text-muted">{{ t(`firewall.service.${r.service}`) }}</span>
            </li>
          </ul>
          <template #footer>
            <UButton icon="i-cg-shield-check" :label="t('firewall.enable')" :loading="busy" @click="enable" />
          </template>
        </PanelSection>
        <div class="grid items-start gap-6 lg:grid-cols-2">
          <PanelSection :title="t('firewall.status')" :description="status && !status.enabled && status.rules?.length && status.rules[0] !== 'ufw is not available' ? t('firewall.inactiveRules') : undefined" flush>
            <template v-if="status" #actions>
              <UBadge :color="status.enabled ? 'success' : 'warning'" variant="subtle" size="lg" :label="status.enabled ? t('firewall.active') : t('firewall.inactive')" />
            </template>
            <USkeleton v-if="!status" class="m-4 h-24" />
            <UEmpty v-else-if="!status.rules?.length" size="sm" icon="i-cg-firewall" :description="t('firewall.noRules')" />
            <ol v-else class="m-0 max-h-[28rem] list-none divide-y divide-default overflow-auto p-0">
              <li v-for="(r, i) in status.rules" :key="i" class="mono flex gap-4 px-4 py-2.5 text-[13px] text-highlighted sm:px-5">
                <span class="w-6 shrink-0 text-dimmed">{{ String(i + 1).padStart(2, '0') }}</span>
                <span class="min-w-0 break-all">{{ r }}</span>
              </li>
            </ol>
          </PanelSection>

          <PanelSection v-if="isAdmin" :title="t('firewall.rule')" :description="t('firewall.ruleHint')">
            <UForm id="fw-form" :state="form" class="flex flex-col gap-4" @submit="apply">
              <div class="grid gap-4 sm:grid-cols-2">
                <UFormField :label="t('firewall.action')">
                  <USelect v-model="form.action" :items="actions" class="w-full" />
                </UFormField>
                <UFormField :label="t('firewall.port')" required>
                  <UInputNumber v-model="form.port" :min="1" :max="65535" class="w-full" placeholder="8080" />
                </UFormField>
              </div>
              <UFormField :label="t('firewall.from')" :hint="t('common.optional')">
                <UInput v-model="form.from" class="w-full font-mono" placeholder="203.0.113.4" />
              </UFormField>
            </UForm>
            <template #footer>
              <UButton type="submit" form="fw-form" :label="t('common.apply')" :disabled="!form.port" :loading="busy" />
            </template>
          </PanelSection>
        </div>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
