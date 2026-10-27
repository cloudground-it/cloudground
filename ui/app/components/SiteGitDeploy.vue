<script setup lang="ts">
/**
 * Git deploy (#24): a push to a branch of the site's repository builds a new
 * release and activates it, the usual rollback standing. The panel makes the
 * webhook's URL and secret and a deploy key for a private repository; the
 * recent deliveries show what each push did (GET/PUT /api/sites/{id}/git-deploy,
 * POST …/secret, …/key and …/run; internal/api SPEC §15).
 */
interface Delivery {
  id: number
  at: string
  forge: string
  event: string
  ref: string
  commit: string
  outcome: 'ping' | 'ignored' | 'queued' | 'coalesced'
  detail: string
  deployment_id: number
  task_id: number
  release_id?: string
  status?: string
}
interface GitDeploy {
  enabled: boolean
  repo: string
  branch: string
  build_command: string
  webhook_path: string
  secret: string
  public_key: string
  deliveries: Delivery[]
}

const { t, locale } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site } = useCurrentSite()

const base = computed(() => `/api/sites/${site.value.id}/git-deploy`)
const { data, refresh } = useLive(() => api.get<GitDeploy>(base.value), {
  on: { types: ['deployment.changed', 'task.finished'], sites: () => site.value.id },
  fallbackMs: 8000
})

const form = reactive({ repo: '', branch: 'main', build: '' })
// Unsaved typing is kept when an event refreshes the data.
const dirty = computed(() => !!data.value && (form.repo.trim() !== data.value.repo || form.branch.trim() !== (data.value.branch || 'main') || form.build.trim() !== data.value.build_command))
let loaded = false
watch(data, (d) => {
  if (d && (!loaded || !dirty.value)) Object.assign(form, { repo: d.repo, branch: d.branch || 'main', build: d.build_command })
  if (d) loaded = true
}, { immediate: true })
const on = computed(() => !!data.value?.enabled)
const hookURL = computed(() => data.value?.webhook_path ? `${window.location.origin}${data.value.webhook_path}` : '')
const showSecret = ref(false)

async function save(enabled: boolean) {
  const body = { enabled, repo: form.repo.trim(), branch: form.branch.trim() || 'main', build_command: form.build.trim() }
  if (await run(() => api.put(base.value, body), t('gitDeploy.saved'))) await refresh()
}
async function newSecret() {
  const ok = await confirm({ title: t('gitDeploy.secretTitle'), description: t('gitDeploy.secretHint'), confirmLabel: t('gitDeploy.secretNew') })
  if (ok && await run(() => api.post(`${base.value}/secret`), t('gitDeploy.secretDone'))) await refresh()
}
async function newKey() {
  const ok = await confirm({ title: t('gitDeploy.keyTitle'), description: t('gitDeploy.keyHint'), confirmLabel: t('gitDeploy.keyNew') })
  if (ok && await run(() => api.post(`${base.value}/key`), t('gitDeploy.keyDone'))) await refresh()
}
async function deployNow() {
  if (await run(() => api.post(`${base.value}/run`), t('gitDeploy.started'))) await refresh()
}

// What each delivery did, in words, with the tone of its badge.
const outcomeColor = { ping: 'neutral', ignored: 'neutral', queued: 'primary', coalesced: 'neutral' } as const
const statusColor = { promoted: 'success', failed: 'error', canceled: 'neutral', superseded: 'neutral', rolled_back: 'warning' } as Record<string, 'success' | 'error' | 'neutral' | 'warning'>
const knownReasons = ['event', 'branch', 'deleted', 'payload']
const short = (c: string) => c.slice(0, 7)
</script>

<template>
  <PanelSection :title="t('gitDeploy.title')" :description="t('gitDeploy.hint')">
    <template #actions>
      <UBadge v-if="data" :color="on ? 'success' : 'neutral'" variant="subtle" :label="on ? t('gitDeploy.on') : t('gitDeploy.off')" />
    </template>
    <div class="flex flex-col gap-4">
      <UFormField :label="t('gitDeploy.repo')" :help="t('gitDeploy.repoHelp')">
        <UInput v-model="form.repo" class="w-full font-mono" maxlength="512" placeholder="git@github.com:owner/repo.git" :disabled="!canManage" />
      </UFormField>
      <div class="grid gap-4 sm:grid-cols-2">
        <UFormField :label="t('gitDeploy.branch')" :help="t('gitDeploy.branchHelp')">
          <UInput v-model="form.branch" class="w-full font-mono" maxlength="100" placeholder="main" :disabled="!canManage" />
        </UFormField>
        <UFormField :label="t('gitDeploy.build')" :help="t('gitDeploy.buildHelp')">
          <UInput v-model="form.build" class="w-full font-mono" maxlength="1024" placeholder="npm ci && npm run build" :disabled="!canManage" />
        </UFormField>
      </div>

      <template v-if="data?.webhook_path">
        <UFormField :label="t('gitDeploy.webhook')" :help="t('gitDeploy.webhookHelp')">
          <div class="flex items-center gap-1">
            <code class="min-w-0 flex-1 truncate font-mono text-xs">{{ hookURL }}</code>
            <CopyButton :value="hookURL" />
          </div>
        </UFormField>
        <UFormField :label="t('gitDeploy.secret')" :help="t('gitDeploy.secretHelp')">
          <div class="flex items-center gap-1">
            <code class="min-w-0 flex-1 truncate font-mono text-xs">{{ showSecret ? data.secret : '•'.repeat(24) }}</code>
            <UButton size="xs" color="neutral" variant="ghost" square :icon="showSecret ? 'i-cg-eye-off' : 'i-cg-eye'" :aria-label="showSecret ? t('gitDeploy.hide') : t('gitDeploy.show')" @click="showSecret = !showSecret" />
            <CopyButton :value="data.secret" />
          </div>
        </UFormField>
        <UFormField :label="t('gitDeploy.deployKey')" :help="t('gitDeploy.deployKeyHelp')">
          <div class="flex items-start gap-1">
            <code class="min-w-0 flex-1 font-mono text-xs [overflow-wrap:anywhere]">{{ data.public_key }}</code>
            <CopyButton :value="data.public_key" />
          </div>
        </UFormField>
        <div v-if="canManage" class="flex flex-wrap gap-2">
          <UButton color="neutral" variant="outline" size="sm" :label="t('gitDeploy.secretNew')" :loading="busy" @click="newSecret" />
          <UButton color="neutral" variant="outline" size="sm" :label="t('gitDeploy.keyNew')" :loading="busy" @click="newKey" />
        </div>
      </template>

      <div v-if="data">
        <h3 class="m-0 mb-2 text-sm font-semibold text-highlighted">
          {{ t('gitDeploy.deliveries') }}
        </h3>
        <p v-if="!data.deliveries.length" class="m-0 text-sm text-muted">
          {{ t('gitDeploy.noDeliveries') }}
        </p>
        <ul v-else class="m-0 list-none divide-y divide-default border border-default p-0">
          <li v-for="d in data.deliveries" :key="d.id" class="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-sm">
            <UBadge :color="outcomeColor[d.outcome] ?? 'neutral'" variant="subtle" :label="t(`gitDeploy.outcome.${d.outcome}`, d.outcome)" />
            <span class="text-muted">{{ d.forge === 'manual' ? t('gitDeploy.manual') : d.forge }}</span>
            <span v-if="d.ref" class="mono text-xs">{{ d.ref.replace('refs/heads/', '') }}</span>
            <span v-if="d.commit" class="mono text-xs text-muted">{{ short(d.commit) }}</span>
            <span v-if="d.detail" class="text-muted">{{ knownReasons.includes(d.detail) ? t(`gitDeploy.reason.${d.detail}`) : d.detail }}</span>
            <NuxtLink v-if="d.release_id" :to="`/sites/${site.id}/deployments`" class="mono text-xs">
              {{ d.release_id }}
            </NuxtLink>
            <UBadge v-if="d.status" :color="statusColor[d.status] ?? 'neutral'" variant="outline" :label="t(`deploy.status.${d.status}`, d.status)" />
            <span class="ms-auto text-xs whitespace-nowrap text-muted" :title="fmtDateTime(d.at, locale)">{{ fmtAgo(d.at, locale) }}</span>
          </li>
        </ul>
      </div>
    </div>
    <template v-if="canManage" #footer>
      <div class="flex flex-wrap gap-2">
        <template v-if="!on">
          <UButton icon="i-cg-git-branch" :label="t('gitDeploy.turnOn')" :disabled="!form.repo.trim()" :loading="busy" @click="save(true)" />
          <UButton v-if="dirty" color="neutral" variant="outline" :label="t('common.save')" :loading="busy" @click="save(false)" />
        </template>
        <template v-else>
          <UButton :label="t('common.save')" :disabled="!dirty || !form.repo.trim()" :loading="busy" @click="save(true)" />
          <UButton icon="i-cg-release" color="neutral" variant="outline" :label="t('gitDeploy.deployNow')" :disabled="dirty" :loading="busy" @click="deployNow" />
          <UButton color="neutral" variant="outline" :label="t('gitDeploy.turnOff')" :loading="busy" @click="save(false)" />
        </template>
      </div>
    </template>
  </PanelSection>
</template>
