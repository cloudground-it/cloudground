<script setup lang="ts">
/**
 * A site's redirects (#83): the name every other name goes to (www or not)
 * and the rules that send a path elsewhere, live once saved and never cached
 * (GET/PUT /api/sites/{id}/redirects; internal/api SPEC §3.18). Serving the
 * other name takes an alias, which also gets the certificate issued again.
 */
interface Rule { match: 'exact' | 'prefix', from: string, to: string, status: 301 | 302 }
interface Redirects { canonical: string, rules: Rule[], names: string[], counterpart: string, counterpart_served: boolean }

const { t } = useI18n()
const api = useApi()
const { canManage, isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const toast = useToast()
const { site, refresh: refreshSite } = useCurrentSite()
const base = computed(() => `/api/sites/${site.value.id}/redirects`)

const { data, refresh } = useLoad(() => api.get<Redirects>(base.value))
const rules = computed(() => data.value?.rules ?? [])

// The canonical host: "" is every name as it is.
const canonical = ref('')
watch(data, (d) => { if (d) canonical.value = d.canonical }, { immediate: true })
const canonicalItems = computed(() => [
  { value: '', label: t('redirects.noCanonical') },
  ...(data.value?.names ?? []).map(n => ({ value: n, label: n }))
])
async function saveCanonical() {
  if (await run(() => api.put(base.value, { canonical: canonical.value }), t('redirects.saved'))) await refresh()
}
// Serving the other name is an alias (administrator): the certificate follows.
async function addCounterpart() {
  const d = data.value
  if (!d) return
  busy.value = true
  try {
    const r = await api.patch<{ warnings?: string[] }>(`/api/sites/${site.value.id}`, { aliases: [...(site.value.aliases ?? []), d.counterpart] })
    toast.add({ title: t('redirects.aliasAdded', { name: d.counterpart }), color: 'success', icon: 'i-cg-check-circle' })
    for (const w of r.warnings ?? []) toast.add({ title: w, color: 'warning', icon: 'i-cg-alert-triangle' })
    await Promise.all([refresh(), refreshSite()])
  } catch (e) {
    toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
  } finally {
    busy.value = false
  }
}

// The rules: the dialog adds or changes one, each save sends the whole list.
const editing = ref(false)
const index = ref(-1)
const form = reactive<Rule>({ match: 'exact', from: '', to: '', status: 301 })
function open(i = -1) {
  index.value = i
  Object.assign(form, i >= 0 ? rules.value[i] : { match: 'exact', from: '', to: '', status: 301 })
  editing.value = true
}
const matchItems = computed(() => [
  { value: 'exact', label: t('redirects.exact') },
  { value: 'prefix', label: t('redirects.prefix') }
])
const statusItems = computed(() => [
  { value: 301, label: t('redirects.permanent') },
  { value: 302, label: t('redirects.temporary') }
])
// The server's rules (site.CheckRedirects), so the button says what it will refuse.
const PATH_RE = /^\/[A-Za-z0-9._~/+,:@!()=*-]*$/
const TARGET_RE = /^(\/(?!\/)[A-Za-z0-9._~/+,:@!()=*%&?-]*|https?:\/\/[a-z0-9.-]+(:\d{1,5})?(\/[A-Za-z0-9._~/+,:@!()=*%&?-]*)?)$/
const segmentsOK = (p: string) => !p.split('/').slice(1).some((s, i, a) => s === '.' || s === '..' || (s === '' && i < a.length - 1))
const fromOK = computed(() => PATH_RE.test(form.from) && form.from.length <= 512 && segmentsOK(form.from)
  && (form.match === 'exact' || (form.from !== '/' && !form.from.endsWith('/'))))
const toOK = computed(() => TARGET_RE.test(form.to) && form.to.length <= 1024 && !(form.match === 'prefix' && form.to.includes('?')))
async function put(next: Rule[], done: string) {
  if (await run(() => api.put(base.value, { rules: next }), done)) {
    editing.value = false
    await refresh()
    return true
  }
  return false
}
function save() {
  const next = rules.value.map(r => ({ ...r }))
  const rule: Rule = { match: form.match, from: form.from, to: form.to, status: form.status }
  if (index.value >= 0) next[index.value] = rule
  else next.push(rule)
  return put(next, t('redirects.saved'))
}
async function remove(i: number) {
  const r = rules.value[i]
  if (!r) return
  if (await confirm({ title: t('redirects.removeTitle', { path: r.from }), description: t('redirects.removeHint'), danger: true, confirmLabel: t('common.remove') })) {
    await put(rules.value.filter((_, k) => k !== i), t('redirects.removed'))
  }
}
const columns = computed(() => [
  { id: 'from', header: t('redirects.from') },
  { id: 'to', header: t('redirects.to') },
  { id: 'status', header: t('redirects.status') },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])
</script>

<template>
  <PanelSection :title="t('redirects.canonicalTitle')" :description="t('redirects.canonicalHint')">
    <div class="flex flex-col gap-4">
      <UFormField :label="t('redirects.canonical')" :help="t('redirects.canonicalHelp')">
        <USelect v-model="canonical" :items="canonicalItems" class="w-full sm:w-96" :disabled="!canManage" />
      </UFormField>
      <UAlert
        v-if="data && !data.counterpart_served"
        color="neutral"
        variant="subtle"
        icon="i-cg-certificate"
        :title="t('redirects.counterpartTitle', { name: data.counterpart })"
        :description="isAdmin ? t('redirects.counterpartHint') : t('redirects.counterpartAdmin')"
        :actions="isAdmin ? [{ label: t('redirects.addCounterpart', { name: data.counterpart }), color: 'neutral', variant: 'outline', onClick: addCounterpart }] : []"
      />
      <UAlert v-if="canonical && isWordPress(site)" color="warning" variant="subtle" icon="i-cg-alert-triangle" :description="t('redirects.wpWarning')" />
    </div>
    <template v-if="canManage" #footer>
      <UButton :label="t('common.save')" :disabled="!data || canonical === data.canonical" :loading="busy" @click="saveCanonical" />
    </template>
  </PanelSection>

  <PanelSection class="mt-6" :title="t('redirects.title')" :description="t('redirects.hint')" flush>
    <template v-if="canManage" #actions>
      <UButton icon="i-cg-plus" :label="t('redirects.add')" @click="open()" />
    </template>
    <UEmpty
      v-if="!rules.length"
      icon="i-cg-external-link"
      :description="t('redirects.none')"
      :actions="canManage ? [{ label: t('redirects.add'), icon: 'i-cg-plus', onClick: () => open() }] : []"
    />
    <UTable v-else :data="rules" :columns="columns">
      <template #from-cell="{ row: { original: r } }">
        <span class="mono text-[13px] text-highlighted">{{ r.from }}</span>
        <UBadge class="ms-2" color="neutral" variant="subtle" :label="r.match === 'prefix' ? t('redirects.prefixBadge') : t('redirects.exactBadge')" />
      </template>
      <template #to-cell="{ row: { original: r } }">
        <span class="mono break-all text-[13px]">{{ r.to }}</span>
      </template>
      <template #status-cell="{ row: { original: r } }">
        <span class="mono text-[13px]">{{ r.status }}</span>
      </template>
      <template #actions-cell="{ row: { index: i } }">
        <div v-if="canManage" class="flex justify-end gap-1">
          <UButton size="sm" color="neutral" variant="ghost" :label="t('redirects.edit')" @click="open(i)" />
          <UButton size="sm" color="error" variant="ghost" square icon="i-cg-trash" :aria-label="t('common.remove')" @click="remove(i)" />
        </div>
      </template>
    </UTable>
  </PanelSection>

  <UModal v-model:open="editing" :title="index >= 0 ? t('redirects.editTitle') : t('redirects.add')" :description="t('redirects.dialogHint')">
    <template #body>
      <div class="flex flex-col gap-4">
        <UFormField :label="t('redirects.matchLabel')" :help="form.match === 'prefix' ? t('redirects.prefixHelp') : t('redirects.exactHelp')">
          <USelect v-model="form.match" :items="matchItems" class="w-full sm:w-64" />
        </UFormField>
        <UFormField :label="t('redirects.from')" :help="t('redirects.fromHelp')" :error="form.from && !fromOK ? t('redirects.fromBad') : undefined">
          <UInput v-model="form.from" class="w-full font-mono" placeholder="/old-page" autocomplete="off" spellcheck="false" />
        </UFormField>
        <UFormField :label="t('redirects.to')" :help="t('redirects.toHelp')" :error="form.to && !toOK ? t('redirects.toBad') : undefined">
          <UInput v-model="form.to" class="w-full font-mono" placeholder="/new-page" autocomplete="off" spellcheck="false" />
        </UFormField>
        <UFormField :label="t('redirects.status')" :help="form.status === 301 ? t('redirects.permanentHelp') : t('redirects.temporaryHelp')">
          <USelect v-model="form.status" :items="statusItems" class="w-full sm:w-64" />
        </UFormField>
      </div>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="editing = false" />
        <UButton color="primary" variant="solid" :label="t('common.save')" :disabled="!fromOK || !toOK" :loading="busy" @click="save" />
      </div>
    </template>
  </UModal>
</template>
