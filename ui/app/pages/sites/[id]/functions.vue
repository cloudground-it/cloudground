<script setup lang="ts">
/**
 * A site's functions (issue 143): small Node.js or Python handlers at
 * https://<domain>/.functions/<name>, each invocation a process of its own
 * with the function's memory and timeout, optionally on a schedule
 * (/api/sites/{id}/functions).
 */
interface Fn {
  name: string, runtime: 'node' | 'python', version: string, memory_mb: number, timeout_sec: number, schedule?: string
  env: Record<string, string>, secrets: string[], release?: string, url: string, code?: string, releases?: string[]
}
interface Invocation { time: string, method: string, status: number, ms: number, source: string }
interface Logs { invocations: Invocation[], output: string[] }
interface InvokeResult { status: number, ms: number, body: string, error?: string }

const MASK = '••••••••'
const { t, locale } = useI18n()
const api = useApi()
const toast = useToast()
const { canManage } = useAuth()
const { busy, run } = useAction()
const { site } = useCurrentSite()

const base = computed(() => `/api/sites/${site.value.id}/functions`)
const { data: list, refresh: refreshList } = useLoad(() => api.get<Fn[]>(base.value))
const selected = ref('')
const fn = ref<Fn>()
const logs = ref<Logs>()
const invoked = ref<InvokeResult>()

interface Row { name: string, value: string }
const form = reactive({ code: '', version: '', memory: 128, timeout: 10, schedule: '', env: [] as Row[], secrets: [] as Row[] })
const creating = reactive({ open: false, name: '', runtime: 'node' as 'node' | 'python' })

async function open(name: string) {
  selected.value = name
  invoked.value = undefined
  const [f, l] = await Promise.all([api.get<Fn>(`${base.value}/${name}`), api.get<Logs>(`${base.value}/${name}/logs`)])
  fn.value = f
  logs.value = l
  Object.assign(form, {
    code: f.code ?? '', version: f.version, memory: f.memory_mb, timeout: f.timeout_sec, schedule: f.schedule ?? '',
    env: Object.entries(f.env ?? {}).map(([n, v]) => ({ name: n, value: v })),
    secrets: (f.secrets ?? []).map(n => ({ name: n, value: MASK }))
  })
}
watch(list, (l) => {
  if (l?.length && !selected.value) void open(l[0]!.name)
}, { immediate: true })

const versions = computed(() => fn.value?.runtime === 'python' ? [...new Set([form.version, '3.12'])] : ['24', '22', '20'])
const NAME_RE = /^[a-z][a-z0-9-]{0,30}$/

async function create() {
  if (!NAME_RE.test(creating.name)) {
    toast.add({ title: t('fn.nameRule'), color: 'error', icon: 'i-cg-alert-circle' })
    return
  }
  if (await run(() => api.post(base.value, { name: creating.name, runtime: creating.runtime }), t('fn.created'))) {
    const name = creating.name
    Object.assign(creating, { open: false, name: '' })
    await refreshList()
    await open(name)
  }
}

async function save() {
  if (!fn.value) return
  const env: Record<string, string> = {}
  for (const r of form.env) if (r.name) env[r.name] = r.value
  const secrets: Record<string, string> = {}
  const kept = new Set(form.secrets.filter(r => r.name).map(r => r.name))
  for (const r of form.secrets) if (r.name) secrets[r.name] = r.value
  for (const n of fn.value.secrets ?? []) if (!kept.has(n)) secrets[n] = ''
  const body: Record<string, unknown> = { version: form.version, memory_mb: Number(form.memory), timeout_sec: Number(form.timeout), schedule: form.schedule.trim(), env, secrets }
  if (form.code !== (fn.value.code ?? '')) body.code = form.code
  if (await run(() => api.put(`${base.value}/${fn.value!.name}`, body), t('fn.saved'))) {
    await refreshList()
    await open(fn.value.name)
  }
}

async function rollback() {
  if (fn.value && await run(() => api.post(`${base.value}/${fn.value!.name}/rollback`, {}), t('fn.rolledBack'))) await open(fn.value.name)
}
async function invoke() {
  if (!fn.value) return
  await run(async () => {
    invoked.value = await api.post<InvokeResult>(`${base.value}/${fn.value!.name}/invoke`, { body: '{}' })
  })
  logs.value = await api.get<Logs>(`${base.value}/${fn.value.name}/logs`)
}
async function remove() {
  if (!fn.value || !confirm(t('fn.deleteConfirm', { name: fn.value.name }))) return
  if (await run(() => api.del(`${base.value}/${fn.value!.name}`), t('fn.deleted'))) {
    selected.value = ''
    fn.value = undefined
    await refreshList()
  }
}
</script>

<template>
  <div class="grid items-start gap-6 lg:grid-cols-[16rem_minmax(0,1fr)]">
    <PanelSection :title="t('fn.title')" :description="t('fn.hint')">
      <ul v-if="list?.length" class="m-0 flex list-none flex-col p-0">
        <li v-for="f in list" :key="f.name">
          <button
            type="button"
            class="flex w-full items-center gap-2 border-b border-default px-1 py-2 text-left text-sm last:border-b-0 hover:bg-elevated"
            :class="selected === f.name ? 'font-semibold text-highlighted' : 'text-muted'"
            @click="open(f.name)"
          >
            <UIcon :name="f.runtime === 'node' ? 'i-brand-node' : 'i-brand-python'" class="size-4 shrink-0" aria-hidden="true" />
            <span class="min-w-0 flex-1 truncate font-mono">{{ f.name }}</span>
            <UIcon v-if="f.schedule" name="i-cg-clock" class="size-3.5 shrink-0" :aria-label="t('fn.scheduled')" />
          </button>
        </li>
      </ul>
      <p v-else-if="list" class="m-0 text-sm text-muted">{{ t('fn.none') }}</p>
      <template v-if="canManage" #footer>
        <div v-if="creating.open" class="flex w-full flex-col gap-2">
          <UInput v-model="creating.name" :placeholder="t('fn.namePlaceholder')" :ui="{ base: 'font-mono' }" maxlength="31" />
          <USelect v-model="creating.runtime" :items="[{ label: 'Node.js', value: 'node' }, { label: 'Python', value: 'python' }]" />
          <div class="flex gap-2">
            <UButton :label="t('fn.create')" :loading="busy" @click="create" />
            <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="creating.open = false" />
          </div>
        </div>
        <UButton v-else icon="i-cg-plus" :label="t('fn.new')" @click="creating.open = true" />
      </template>
    </PanelSection>

    <div v-if="fn" class="flex min-w-0 flex-col gap-6">
      <PanelSection :title="fn.name" :description="t('fn.endpoint')">
        <template #actions>
          <UBadge color="neutral" variant="subtle" :label="`${fn.runtime === 'node' ? 'Node.js' : 'Python'} ${fn.version}`" />
        </template>
        <div class="flex flex-col gap-4">
          <div class="flex items-center gap-1">
            <code class="min-w-0 flex-1 truncate font-mono text-xs">{{ fn.url }}</code>
            <CopyButton :value="fn.url" />
          </div>
          <UFormField :label="t('fn.code')" :help="t(fn.runtime === 'node' ? 'fn.codeHelpNode' : 'fn.codeHelpPython')">
            <UTextarea v-model="form.code" :rows="16" class="w-full" :ui="{ base: 'font-mono text-xs' }" spellcheck="false" :disabled="!canManage" />
          </UFormField>
          <div class="grid gap-4 sm:grid-cols-4">
            <UFormField :label="t('app.runtime')">
              <USelect v-model="form.version" :items="versions.map(v => ({ label: v, value: v }))" class="w-full" :disabled="!canManage" />
            </UFormField>
            <UFormField :label="t('fn.memory')" hint="MB">
              <UInputNumber v-model="form.memory" :min="64" :max="1024" :step="64" class="w-full" :disabled="!canManage" />
            </UFormField>
            <UFormField :label="t('fn.timeout')" :hint="t('fn.seconds')">
              <UInputNumber v-model="form.timeout" :min="1" :max="300" class="w-full" :disabled="!canManage" />
            </UFormField>
            <UFormField :label="t('fn.schedule')" :help="t('fn.scheduleHelp')">
              <UInput v-model="form.schedule" placeholder="*/15 * * * *" :ui="{ base: 'font-mono' }" class="w-full" :disabled="!canManage" />
            </UFormField>
          </div>
          <div class="flex flex-col gap-2">
            <h3 class="m-0 text-sm font-semibold text-highlighted">{{ t('app.variables') }}</h3>
            <div v-for="(r, i) in form.env" :key="`e${i}`" class="grid grid-cols-[minmax(0,12rem)_minmax(0,1fr)_auto] gap-2">
              <UInput v-model="r.name" :placeholder="t('app.name')" :ui="{ base: 'font-mono uppercase' }" :aria-label="t('app.name')" :disabled="!canManage" />
              <UInput v-model="r.value" :placeholder="t('app.value')" :ui="{ base: 'font-mono' }" :aria-label="t('app.value')" :disabled="!canManage" />
              <UButton v-if="canManage" color="neutral" variant="ghost" square icon="i-cg-trash" :aria-label="t('common.remove')" @click="form.env.splice(i, 1)" />
            </div>
            <UButton v-if="canManage" color="neutral" variant="outline" size="sm" icon="i-cg-plus" :label="t('app.addVariable')" class="self-start" @click="form.env.push({ name: '', value: '' })" />
            <h3 class="m-0 mt-2 text-sm font-semibold text-highlighted">{{ t('app.secrets') }}</h3>
            <div v-for="(r, i) in form.secrets" :key="`s${i}`" class="grid grid-cols-[minmax(0,12rem)_minmax(0,1fr)_auto] gap-2">
              <UInput v-model="r.name" :placeholder="t('app.name')" :ui="{ base: 'font-mono uppercase' }" :aria-label="t('app.name')" :disabled="!canManage" />
              <UInput v-model="r.value" type="password" autocomplete="off" :placeholder="t('app.value')" :ui="{ base: 'font-mono' }" :aria-label="t('app.value')" :disabled="!canManage" />
              <UButton v-if="canManage" color="neutral" variant="ghost" square icon="i-cg-trash" :aria-label="t('common.remove')" @click="form.secrets.splice(i, 1)" />
            </div>
            <UButton v-if="canManage" color="neutral" variant="outline" size="sm" icon="i-cg-plus" :label="t('app.addSecret')" class="self-start" @click="form.secrets.push({ name: '', value: '' })" />
          </div>
        </div>
        <template v-if="canManage" #footer>
          <div class="flex flex-wrap gap-2">
            <UButton :label="t('fn.deploy')" :loading="busy" @click="save" />
            <UButton color="neutral" variant="outline" icon="i-cg-play" :label="t('fn.test')" :disabled="busy" @click="invoke" />
            <UButton color="neutral" variant="outline" icon="i-cg-rollback" :label="t('fn.rollback')" :disabled="busy || (fn.releases?.length ?? 0) < 2" @click="rollback" />
            <UButton color="error" variant="ghost" icon="i-cg-trash" :label="t('common.delete')" :disabled="busy" @click="remove" />
          </div>
        </template>
      </PanelSection>

      <PanelSection :title="t('fn.invocations')" :description="t('fn.invocationsHint')">
        <UAlert v-if="invoked" :color="invoked.status > 0 && invoked.status < 500 ? 'success' : 'error'" variant="subtle" class="mb-3"
          :title="invoked.status ? t('fn.testResult', { status: invoked.status, ms: invoked.ms }) : t('fn.testFailed')"
          :description="invoked.error || invoked.body" />
        <table v-if="logs?.invocations.length" class="w-full text-sm">
          <thead>
            <tr class="text-left text-[13px] text-muted">
              <th class="pb-2 font-normal">{{ t('fn.when') }}</th>
              <th class="pb-2 font-normal">{{ t('fn.source') }}</th>
              <th class="pb-2 text-right font-normal">{{ t('fn.status') }}</th>
              <th class="pb-2 text-right font-normal">{{ t('fn.duration') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(i, n) in logs.invocations.slice(0, 30)" :key="n" class="border-t border-default">
              <td class="py-1.5">{{ fmtDateTime(i.time, locale) }}</td>
              <td class="py-1.5 text-muted">{{ t(`fn.src.${i.source}`) }}</td>
              <td class="py-1.5 text-right tabular-nums" :class="i.status >= 500 ? 'text-cg-err' : ''">{{ i.status }}</td>
              <td class="py-1.5 text-right tabular-nums">{{ i.ms }} ms</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="m-0 text-sm text-muted">{{ t('fn.noInvocations') }}</p>
        <details v-if="logs?.output.length" class="mt-4">
          <summary class="cursor-pointer text-sm text-muted">{{ t('fn.output') }}</summary>
          <pre class="mt-2 max-h-72 overflow-auto bg-muted p-3 font-mono text-xs">{{ logs.output.join('\n') }}</pre>
        </details>
      </PanelSection>
    </div>
  </div>
</template>
