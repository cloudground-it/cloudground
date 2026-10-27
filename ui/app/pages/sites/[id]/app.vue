<script setup lang="ts">
/**
 * A Node.js or Python site's app (#28): its health and restarts, the
 * runtime version and start command, and its environment, plain variables
 * shown and secrets sealed (GET and PUT /api/sites/{id}/app,
 * /app/status, /app/restart).
 */
import type { SiteApp } from '~/types'

interface AppView extends SiteApp { socket: string }
interface AppStatus { active: boolean, state: string, since?: string, restarts: number, last_exit: number, health: { status: number, ms: number, error?: string } }

const MASK = '••••••••'
const { t, locale } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const { site, refresh: refreshSite } = useCurrentSite()

const base = computed(() => `/api/sites/${site.value.id}/app`)
const { data, refresh } = useLoad(() => api.get<AppView>(base.value))
const { data: status, refresh: refreshStatus } = useLoad(() => api.get<AppStatus>(`${base.value}/status`))
useIntervalFn(() => void refreshStatus(), 15000)

interface Row { name: string, value: string }
const form = reactive({ version: '', start: '', port: 0, health: '', env: [] as Row[], secrets: [] as Row[] })
watch(data, (a) => {
  if (!a) return
  Object.assign(form, {
    version: a.version, start: a.start, port: a.port ?? 0, health: a.health_path ?? '',
    env: Object.entries(a.env ?? {}).map(([name, value]) => ({ name, value })),
    secrets: (a.secrets ?? []).map(name => ({ name, value: MASK }))
  })
}, { immediate: true })

const runtime = computed(() => site.value.type === 'node' ? 'Node.js' : 'Python')
const versions = computed(() => site.value.type === 'node' ? ['24', '22', '20'] : [...new Set([form.version, '3.12'])])
const NAME_RE = /^[A-Z_][A-Z0-9_]{0,63}$/
const badName = computed(() => [...form.env, ...form.secrets].find(r => r.name && !NAME_RE.test(r.name))?.name)

const healthy = computed(() => !!status.value?.active && status.value.health.status > 0 && status.value.health.status < 500)
const stateLabel = computed(() => {
  const s = status.value
  if (!s) return ''
  if (!s.active) return t('app.down')
  return healthy.value ? t('app.healthy') : t('app.unhealthy')
})

async function save() {
  const env: Record<string, string> = {}
  for (const r of form.env) if (r.name) env[r.name] = r.value
  const secrets: Record<string, string> = {}
  const kept = new Set(form.secrets.filter(r => r.name).map(r => r.name))
  for (const r of form.secrets) if (r.name) secrets[r.name] = r.value
  for (const n of data.value?.secrets ?? []) if (!kept.has(n)) secrets[n] = ''
  const body = { version: form.version, start: form.start, port: Number(form.port) || 0, health_path: form.health, env, secrets }
  if (await run(() => api.put(base.value, body), t('app.saved'))) {
    await Promise.all([refresh(), refreshSite(), refreshStatus()])
  }
}
function restart() {
  void run(() => api.post(`${base.value}/restart`), t('app.restarted')).then(() => refreshStatus())
}
</script>

<template>
  <div class="grid items-start gap-6 lg:grid-cols-2">
    <PanelSection :title="t('app.statusTitle')" :description="t('app.statusHint', { runtime })">
      <template #actions>
        <UBadge v-if="status" :color="healthy ? 'success' : status.active ? 'warning' : 'error'" variant="subtle" :label="stateLabel" />
      </template>
      <USkeleton v-if="!status" class="h-24" />
      <dl v-else class="m-0 grid grid-cols-[auto_minmax(0,1fr)] gap-x-6 gap-y-2 text-sm">
        <dt class="text-muted">{{ t('app.unitState') }}</dt>
        <dd class="m-0 font-mono">{{ status.state }}</dd>
        <template v-if="status.since">
          <dt class="text-muted">{{ t('app.since') }}</dt>
          <dd class="m-0">{{ fmtDateTime(status.since, locale) }}</dd>
        </template>
        <dt class="text-muted">{{ t('app.restarts') }}</dt>
        <dd class="m-0 tabular-nums">{{ status.restarts }}<span v-if="status.last_exit" class="text-muted"> · {{ t('app.lastExit', { code: status.last_exit }) }}</span></dd>
        <dt class="text-muted">{{ t('app.health') }}</dt>
        <dd class="m-0">
          <template v-if="status.health.status">HTTP {{ status.health.status }} · {{ status.health.ms }} ms</template>
          <template v-else>{{ status.health.error || '—' }}</template>
        </dd>
      </dl>
      <template #footer>
        <div class="flex flex-wrap gap-2">
          <UButton v-if="canManage" icon="i-cg-refresh" :label="t('app.restart')" :loading="busy" @click="restart" />
          <UButton color="neutral" variant="outline" icon="i-cg-logs" :label="t('app.logs')" :to="`/sites/${site.id}/logs?source=app`" />
        </div>
      </template>
    </PanelSection>

    <PanelSection :title="t('app.settingsTitle')" :description="t('app.settingsHint')">
      <div class="flex flex-col gap-4">
        <UFormField :label="t('app.runtime')" :help="t('app.versionHelp')">
          <USelect v-model="form.version" :items="versions.map(v => ({ label: `${runtime} ${v}`, value: v }))" class="w-56" :disabled="!canManage" />
        </UFormField>
        <UFormField :label="t('app.start')" :help="t('app.startHelp')">
          <UInput v-model="form.start" class="w-full" :ui="{ base: 'font-mono' }" maxlength="1024" spellcheck="false" :disabled="!canManage" />
        </UFormField>
        <div class="grid gap-4 sm:grid-cols-2">
          <UFormField :label="t('app.port')" :help="t('app.portHelp')">
            <UInputNumber v-model="form.port" :min="0" :max="65535" class="w-40" :disabled="!canManage" />
          </UFormField>
          <UFormField :label="t('app.healthPath')" :help="t('app.healthPathHelp')">
            <UInput v-model="form.health" placeholder="/" class="w-full" :ui="{ base: 'font-mono' }" :disabled="!canManage" />
          </UFormField>
        </div>
        <p v-if="data" class="m-0 text-xs text-muted">{{ t('app.socket', { socket: data.socket }) }}</p>
      </div>
    </PanelSection>

    <PanelSection :title="t('app.envTitle')" :description="t('app.envHint')" class="lg:col-span-2">
      <div class="flex flex-col gap-6">
        <div class="flex flex-col gap-2">
          <h3 class="m-0 text-sm font-semibold text-highlighted">{{ t('app.variables') }}</h3>
          <div v-for="(r, i) in form.env" :key="`e${i}`" class="grid grid-cols-[minmax(0,14rem)_minmax(0,1fr)_auto] gap-2">
            <UInput v-model="r.name" :placeholder="t('app.name')" class="font-mono" :ui="{ base: 'font-mono uppercase' }" :aria-label="t('app.name')" :disabled="!canManage" />
            <UInput v-model="r.value" :placeholder="t('app.value')" :ui="{ base: 'font-mono' }" :aria-label="t('app.value')" :disabled="!canManage" />
            <UButton v-if="canManage" color="neutral" variant="ghost" square icon="i-cg-trash" :aria-label="t('common.remove')" @click="form.env.splice(i, 1)" />
          </div>
          <UButton v-if="canManage" color="neutral" variant="outline" size="sm" icon="i-cg-plus" :label="t('app.addVariable')" class="self-start" @click="form.env.push({ name: '', value: '' })" />
        </div>
        <div class="flex flex-col gap-2">
          <h3 class="m-0 text-sm font-semibold text-highlighted">{{ t('app.secrets') }}</h3>
          <p class="m-0 text-[13px] text-muted">{{ t('app.secretsHint') }}</p>
          <div v-for="(r, i) in form.secrets" :key="`s${i}`" class="grid grid-cols-[minmax(0,14rem)_minmax(0,1fr)_auto] gap-2">
            <UInput v-model="r.name" :placeholder="t('app.name')" :ui="{ base: 'font-mono uppercase' }" :aria-label="t('app.name')" :disabled="!canManage" />
            <UInput v-model="r.value" type="password" autocomplete="off" :placeholder="t('app.value')" :ui="{ base: 'font-mono' }" :aria-label="t('app.value')" :disabled="!canManage" />
            <UButton v-if="canManage" color="neutral" variant="ghost" square icon="i-cg-trash" :aria-label="t('common.remove')" @click="form.secrets.splice(i, 1)" />
          </div>
          <UButton v-if="canManage" color="neutral" variant="outline" size="sm" icon="i-cg-plus" :label="t('app.addSecret')" class="self-start" @click="form.secrets.push({ name: '', value: '' })" />
        </div>
        <UAlert v-if="badName" color="error" variant="subtle" icon="i-cg-alert-circle" :description="t('app.nameRule', { name: badName })" />
      </div>
      <template v-if="canManage" #footer>
        <UButton :label="t('app.saveRestart')" :loading="busy" :disabled="!!badName || !form.start.trim()" @click="save" />
      </template>
    </PanelSection>
  </div>
</template>
