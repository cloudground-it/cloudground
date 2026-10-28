<script setup lang="ts">
/**
 * A function's settings (PUT /api/functions/{name}): runtime version,
 * memory, timeout and schedule; its environment, variables shown and
 * secrets sealed (a secret sent back as ••••••••  is kept, "" removes it, as
 * a managed app's); and deleting it.
 */
import type { Runtimes } from '~/types'

const { t } = useI18n()
const api = useApi()
const { isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { fn, base, refresh } = useCurrentFunction()

interface RuntimesView { installed: Runtimes, available: { node: string[], python: string[] } }
const { data: runtimes } = useLoad(() => isAdmin.value ? api.get<RuntimesView>('/api/runtimes') : Promise.resolve(null))

interface Row { name: string, value: string }
const form = reactive({ version: '', memory: 128, timeout: 10, schedule: '', env: [] as Row[], secrets: [] as Row[] })
function load() {
  const f = fn.value
  Object.assign(form, {
    version: f.version, memory: f.memory_mb, timeout: f.timeout_sec, schedule: f.schedule ?? '',
    env: Object.entries(f.env ?? {}).map(([name, value]) => ({ name, value })),
    secrets: (f.secrets ?? []).map(name => ({ name, value: SECRET_MASK }))
  })
}
watch(fn, load, { immediate: true })

// The installed versions; the function's own stays listed even if it is not.
const versionItems = computed(() => {
  const have = runtimes.value?.installed[fn.value.runtime]?.map(i => i.version) ?? []
  return [...new Set([fn.value.version, ...have])]
    .sort((a, b) => b.localeCompare(a, undefined, { numeric: true }))
    .map(v => ({ value: v, label: `${fnRuntimeName(fn.value.runtime)} ${v}` }))
})

const NAME_RE = /^[A-Z_][A-Z0-9_]{0,63}$/
const badName = computed(() => [...form.env, ...form.secrets].find(r => r.name && !NAME_RE.test(r.name))?.name)

async function save() {
  const env: Record<string, string> = {}
  for (const r of form.env) if (r.name) env[r.name] = r.value
  const secrets: Record<string, string> = {}
  const kept = new Set(form.secrets.filter(r => r.name).map(r => r.name))
  for (const r of form.secrets) if (r.name) secrets[r.name] = r.value
  for (const n of fn.value.secrets ?? []) if (!kept.has(n)) secrets[n] = ''
  const body = { version: form.version, memory_mb: Number(form.memory), timeout_sec: Number(form.timeout), schedule: form.schedule.trim(), env, secrets }
  if (await run(() => api.put(base.value, body), t('fn.settings.saved'))) await refresh()
}

async function remove() {
  const name = fn.value.name
  const ok = await confirm({ title: t('fn.settings.deleteTitle', { name }), description: t('fn.settings.deleteHint'), danger: true, confirmLabel: t('common.delete'), typed: name })
  if (ok && await run(() => api.del(base.value), t('fn.settings.deleted'))) await navigateTo('/functions')
}
</script>

<template>
  <div class="flex flex-col gap-6">
    <PanelSection :title="t('fn.settings.title')" :description="t('fn.settings.runHint')" icon="i-cg-sliders">
      <div class="grid max-w-4xl gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <UFormField :label="t('fn.create.version')" :help="t('fn.settings.versionHelp')">
          <USelect v-model="form.version" :items="versionItems" class="w-full" :disabled="!isAdmin" />
        </UFormField>
        <UFormField :label="t('fn.settings.memory')" hint="64–1024 MB">
          <UInputNumber v-model="form.memory" :min="64" :max="1024" :step="64" class="w-full" :disabled="!isAdmin" />
        </UFormField>
        <UFormField :label="t('fn.settings.timeout')" hint="1–300 s">
          <UInputNumber v-model="form.timeout" :min="1" :max="300" class="w-full" :disabled="!isAdmin" />
        </UFormField>
        <UFormField :label="t('fn.settings.schedule')" :help="t('fn.settings.scheduleHelp')">
          <UInput v-model="form.schedule" placeholder="*/15 * * * *" class="w-full font-mono" :disabled="!isAdmin" />
        </UFormField>
      </div>
      <template v-if="isAdmin" #footer>
        <UButton :label="t('common.save')" :loading="busy" :disabled="!!badName" @click="save" />
      </template>
    </PanelSection>

    <PanelSection :title="t('app.envTitle')" :description="t('fn.settings.envHint')" icon="i-cg-terminal">
      <div class="flex max-w-3xl flex-col gap-2">
        <h3 class="m-0 text-sm font-semibold text-highlighted">{{ t('app.variables') }}</h3>
        <div v-for="(r, i) in form.env" :key="`e${i}`" class="grid grid-cols-[minmax(0,12rem)_minmax(0,1fr)_auto] gap-2">
          <UInput v-model="r.name" :placeholder="t('app.name')" :ui="{ base: 'font-mono uppercase' }" :aria-label="t('app.name')" :disabled="!isAdmin" />
          <UInput v-model="r.value" :placeholder="t('app.value')" :ui="{ base: 'font-mono' }" :aria-label="t('app.value')" :disabled="!isAdmin" />
          <UButton v-if="isAdmin" color="neutral" variant="ghost" square icon="i-cg-trash" :aria-label="t('common.remove')" @click="form.env.splice(i, 1)" />
        </div>
        <UButton v-if="isAdmin" color="neutral" variant="outline" size="sm" icon="i-cg-plus" :label="t('app.addVariable')" class="self-start" @click="form.env.push({ name: '', value: '' })" />
        <h3 class="m-0 mt-3 text-sm font-semibold text-highlighted">{{ t('app.secrets') }}</h3>
        <p class="m-0 text-[13px] text-muted">{{ t('fn.settings.secretsHint') }}</p>
        <div v-for="(r, i) in form.secrets" :key="`s${i}`" class="grid grid-cols-[minmax(0,12rem)_minmax(0,1fr)_auto] gap-2">
          <UInput v-model="r.name" :placeholder="t('app.name')" :ui="{ base: 'font-mono uppercase' }" :aria-label="t('app.name')" :disabled="!isAdmin" />
          <UInput v-model="r.value" type="password" autocomplete="off" :placeholder="t('app.value')" :ui="{ base: 'font-mono' }" :aria-label="t('app.value')" :disabled="!isAdmin" />
          <UButton v-if="isAdmin" color="neutral" variant="ghost" square icon="i-cg-trash" :aria-label="t('common.remove')" @click="form.secrets.splice(i, 1)" />
        </div>
        <UButton v-if="isAdmin" color="neutral" variant="outline" size="sm" icon="i-cg-plus" :label="t('app.addSecret')" class="self-start" @click="form.secrets.push({ name: '', value: '' })" />
        <p v-if="badName" class="m-0 text-[13px] text-error">{{ t('app.nameRule', { name: badName }) }}</p>
      </div>
      <template v-if="isAdmin" #footer>
        <UButton :label="t('common.save')" :loading="busy" :disabled="!!badName" @click="save" />
        <span class="text-[13px] text-muted">{{ t('fn.settings.saveHint') }}</span>
      </template>
    </PanelSection>

    <PanelSection v-if="isAdmin" :title="t('fn.settings.dangerTitle')" icon="i-cg-trash">
      <div class="flex flex-wrap items-center justify-between gap-4">
        <p class="m-0 max-w-[60ch] text-sm text-muted">{{ t('fn.settings.deleteHint') }}</p>
        <UButton color="error" variant="outline" icon="i-cg-trash" :label="t('fn.settings.delete')" :disabled="busy" @click="remove" />
      </div>
    </PanelSection>
  </div>
</template>
