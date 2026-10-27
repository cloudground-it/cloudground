<script setup lang="ts">
/**
 * A new function (POST /api/functions): its name, runtime and version, the
 * starter code (an HTTP handler or a scheduled job) and who may call it. A
 * function with a token gets it in the answer, once: it is shown here before
 * moving on to the function.
 */
import type { Runtimes } from '~/types'
import type { FnAuth, FnRuntime, FnView } from '~/utils/functions'

definePageMeta({
  middleware: [() => {
    if (!useAuth().isAdmin.value) return navigateTo('/functions', { replace: true })
  }]
})

const { t } = useI18n()
const api = useApi()
const toast = useToast()
useHead({ title: () => t('fn.new') })

interface RuntimesView { installed: Runtimes, available: { node: string[], python: string[] } }
const { data: runtimes } = useLoad(() => api.get<RuntimesView>('/api/runtimes'))

type Template = 'http' | 'scheduled'
const form = reactive({ name: '', runtime: 'node' as FnRuntime, version: '', template: 'http' as Template, schedule: '0 * * * *', auth: 'public' as FnAuth })

const installed = (r: FnRuntime, v: string) => !!runtimes.value?.installed[r]?.some(i => i.version === v)
const versions = computed(() => {
  const r = runtimes.value
  if (!r) return []
  return [...new Set([...r.installed[form.runtime].map(i => i.version), ...r.available[form.runtime]])]
    .sort((a, b) => b.localeCompare(a, undefined, { numeric: true }))
})
// The newest installed version, else the newest offered.
watch([() => form.runtime, runtimes], () => {
  if (!versions.value.includes(form.version)) form.version = versions.value.find(v => installed(form.runtime, v)) ?? versions.value[0] ?? ''
}, { immediate: true })
const versionItems = computed(() => versions.value.map(v => ({
  value: v,
  label: `${fnRuntimeName(form.runtime)} ${v}${installed(form.runtime, v) ? '' : ` · ${t('fn.create.notInstalled')}`}`
})))
const missing = computed(() => !!form.version && !!runtimes.value && !installed(form.runtime, form.version))

const runtimeItems = computed(() => (['node', 'python'] as FnRuntime[]).map(r => ({ value: r, label: fnRuntimeName(r), description: t(`fn.create.entry`, { file: fnEntry(r) }) })))
const templateItems = computed(() => (['http', 'scheduled'] as Template[]).map(v => ({ value: v, label: t(`fn.create.template.${v}`), description: t(`fn.create.template.${v}Hint`) })))
const authItems = computed(() => (['public', 'token'] as FnAuth[]).map(v => ({ value: v, label: t(`fn.auth.${v}`), description: t(`fn.auth.${v}Hint`) })))

const nameError = computed(() => form.name && !FN_NAME_RE.test(form.name) ? t('fn.create.nameRule') : undefined)
const refused = ref('')
const busy = ref(false)
const made = ref<FnView | null>(null)

async function create() {
  if (!FN_NAME_RE.test(form.name)) {
    refused.value = t('fn.create.nameRule')
    return
  }
  busy.value = true
  refused.value = ''
  try {
    const body: Record<string, unknown> = { name: form.name, runtime: form.runtime, template: form.template, auth: form.auth }
    if (form.version) body.version = form.version
    if (form.template === 'scheduled' && form.schedule.trim()) body.schedule = form.schedule.trim()
    const f = await api.post<FnView>('/api/functions', body)
    toast.add({ title: t('fn.create.done'), color: 'success', icon: 'i-cg-check-circle' })
    if (f.token) made.value = f
    else await navigateTo(`/functions/${f.name}`)
  } catch (e) {
    refused.value = errorMessage(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <UDashboardPanel id="function-new">
    <template #header>
      <PageNavbar :title="t('fn.new')" :section="t('fn.title')" section-to="/functions" />
    </template>

    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('fn.new')" :description="t('fn.create.intro')" />

        <PanelSection v-if="made?.token" :title="made.name" icon="i-cg-webhook">
          <FnTokenOnce :token="made.token" @done="navigateTo(`/functions/${made.name}`)" />
          <template #footer>
            <UButton trailing-icon="i-cg-arrow-right" :label="t('fn.create.open')" :to="`/functions/${made.name}`" />
          </template>
        </PanelSection>

        <PanelSection v-else :title="t('fn.create.title')" icon="i-cg-webhook">
          <div class="flex max-w-3xl flex-col gap-5">
            <UFormField :label="t('fn.create.name')" :help="nameError ? undefined : t('fn.create.nameHelp')" :error="nameError" required>
              <UInput v-model="form.name" placeholder="contact-form" maxlength="31" autocomplete="off" spellcheck="false" class="w-full font-mono sm:w-96" autofocus @keydown.enter="create" />
            </UFormField>

            <URadioGroup
              v-model="form.runtime"
              :legend="t('fn.create.runtime')"
              :items="runtimeItems"
              variant="card"
              orientation="horizontal"
              :ui="{ legend: 'mb-2 text-sm font-medium text-highlighted', fieldset: 'flex-wrap gap-2', item: 'min-w-48 flex-1' }"
            />

            <UFormField :label="t('fn.create.version')">
              <USelect v-model="form.version" :items="versionItems" :loading="!runtimes" class="w-full sm:w-96" />
            </UFormField>
            <UAlert
              v-if="missing"
              color="warning"
              variant="subtle"
              icon="i-cg-alert-triangle"
              :description="t('fn.create.installFirst', { runtime: fnRuntimeName(form.runtime), version: form.version })"
              :actions="[{ label: t('fn.create.openRuntimes'), to: '/services#runtimes', color: 'neutral', variant: 'outline', size: 'sm' }]"
            />

            <URadioGroup
              v-model="form.template"
              :legend="t('fn.create.starter')"
              :items="templateItems"
              variant="card"
              orientation="horizontal"
              :ui="{ legend: 'mb-2 text-sm font-medium text-highlighted', fieldset: 'flex-wrap gap-2', item: 'min-w-48 flex-1' }"
            />
            <UFormField v-if="form.template === 'scheduled'" :label="t('fn.settings.schedule')" :help="t('fn.settings.scheduleHelp')">
              <UInput v-model="form.schedule" placeholder="0 * * * *" class="w-full font-mono sm:w-60" />
            </UFormField>

            <URadioGroup
              v-model="form.auth"
              :legend="t('fn.create.access')"
              :items="authItems"
              variant="card"
              orientation="horizontal"
              :ui="{ legend: 'mb-2 text-sm font-medium text-highlighted', fieldset: 'flex-wrap gap-2', item: 'min-w-48 flex-1' }"
            />

            <UAlert v-if="refused" color="error" variant="subtle" icon="i-cg-alert-circle" :title="t('common.failed')">
              <template #description>
                <pre class="m-0 max-h-72 overflow-auto font-mono text-xs whitespace-pre-wrap">{{ refused }}</pre>
              </template>
            </UAlert>
          </div>
          <template #footer>
            <UButton icon="i-cg-plus" :label="t('fn.create.submit')" :loading="busy" :disabled="!form.name || !!nameError" @click="create" />
            <UButton color="neutral" variant="ghost" :label="t('common.cancel')" to="/functions" />
          </template>
        </PanelSection>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
