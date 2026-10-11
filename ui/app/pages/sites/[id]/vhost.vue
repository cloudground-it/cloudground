<script setup lang="ts">
/**
 * A site's Vhost tab (R-5.4, internal/engine SPEC §7): the site's routing
 * block, freely editable by an administrator, between the parts of the
 * vhost the panel manages (TLS, cache and bypass, security headers, the
 * PHP link, limits), shown read-only. nginx tests every save; a refusal
 * leaves the site serving what it served.
 */
interface RoutingView {
  routing: string
  default: string
  custom: boolean
  editable: boolean
  before: string
  after: string
}

const { t } = useI18n()
const api = useApi()
const { isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const toast = useToast()
const { site } = useCurrentSite()

const view = ref<RoutingView | null>(null)
const loadError = ref<string | null>(null)
// Bumped after a save or a restore, so the editor starts clean on what is live.
const generation = ref(0)
const dirty = ref(false)
const refusal = ref<string | null>(null)
const showBefore = ref(false)
const showAfter = ref(false)
const editor = ref<{ save: () => Promise<boolean> } | null>(null)

async function load() {
  try {
    view.value = await api.get<RoutingView>(`/api/sites/${site.value.id}/routing`)
    loadError.value = null
  } catch (e) {
    loadError.value = errorMessage(e)
  }
}
if (isAdmin.value) await load()

async function put(routing: string): Promise<boolean> {
  try {
    view.value = await api.put<RoutingView>(`/api/sites/${site.value.id}/routing`, { routing })
    refusal.value = null
    generation.value++
    return true
  } catch (e) {
    // The refusal stays on the page, next to the text it is about.
    refusal.value = errorMessage(e)
    return false
  }
}

async function save(content: string): Promise<boolean> {
  let ok = false
  await run(async () => {
    ok = await put(content)
    if (!ok) throw new Error(refusal.value ?? '')
  }, t('vhost.saved'))
  return ok
}

async function restore() {
  const ok = await confirm({ title: t('vhost.restoreTitle'), description: t('vhost.restoreHint'), danger: true, confirmLabel: t('vhost.restore') })
  if (!ok) return
  if (await put('')) toast.add({ title: t('vhost.restored'), color: 'success', icon: 'i-cg-check-circle' })
  else toast.add({ title: t('common.failed'), description: refusal.value ?? '', color: 'error', icon: 'i-cg-alert-circle' })
}

const lines = (s: string) => s ? s.replace(/\n$/, '').split('\n').length : 0

onBeforeRouteLeave(async () => {
  if (!dirty.value) return true
  return confirm({ title: t('editor.discardTitle'), description: t('editor.discardBody'), danger: true, confirmLabel: t('editor.discard') })
})
</script>

<template>
  <UAlert v-if="!isAdmin" color="neutral" variant="subtle" icon="i-cg-lock" :description="t('vhost.adminOnly')" />
  <UAlert v-else-if="loadError" color="error" variant="subtle" icon="i-cg-alert-circle" :title="t('vhost.loadFailed')" :description="loadError" />
  <PanelSection v-else-if="view" :title="t('vhost.title')" :description="t('vhost.hint')" :flush="view.editable">
    <template #actions>
      <UBadge :color="view.custom ? 'info' : 'neutral'" variant="subtle" size="lg" :label="view.custom ? t('vhost.custom') : t('vhost.default')" />
    </template>

    <UAlert v-if="!view.editable" color="neutral" variant="subtle" icon="i-cg-info" :description="t('vhost.notEditable')" />
    <template v-else>
      <UCollapsible v-model:open="showBefore" :ui="{ content: 'border-b border-default' }">
        <UButton
          block
          color="neutral"
          variant="ghost"
          class="justify-between border-b border-default bg-elevated px-4 sm:px-5"
          :label="t('vhost.panelPart', { n: lines(view.before) })"
          :trailing-icon="showBefore ? 'i-cg-chevron-up' : 'i-cg-chevron-down'"
        />
        <template #content>
          <pre class="m-0 max-h-80 overflow-auto bg-elevated px-4 py-3 font-mono text-xs leading-relaxed text-muted sm:px-5" :aria-label="t('vhost.readOnly')">{{ view.before }}</pre>
        </template>
      </UCollapsible>

      <div class="h-[min(60vh,520px)] min-h-72">
        <LazyCodeEditor
          :key="generation"
          ref="editor"
          v-model:dirty="dirty"
          :content="view.routing"
          filename="routing.conf"
          :save="save"
        />
      </div>

      <UCollapsible v-model:open="showAfter" :ui="{ content: 'border-t border-default' }">
        <UButton
          block
          color="neutral"
          variant="ghost"
          class="justify-between border-t border-default bg-elevated px-4 sm:px-5"
          :label="t('vhost.panelPart', { n: lines(view.after) })"
          :trailing-icon="showAfter ? 'i-cg-chevron-up' : 'i-cg-chevron-down'"
        />
        <template #content>
          <pre class="m-0 max-h-80 overflow-auto bg-elevated px-4 py-3 font-mono text-xs leading-relaxed text-muted sm:px-5" :aria-label="t('vhost.readOnly')">{{ view.after }}</pre>
        </template>
      </UCollapsible>
    </template>

    <template v-if="view.editable" #footer>
      <UButton :label="t('common.save')" :loading="busy" :disabled="!dirty" @click="editor?.save()" />
      <UButton
        color="neutral"
        variant="outline"
        icon="i-cg-restore"
        :label="t('vhost.restore')"
        :disabled="busy || (!view.custom && !dirty)"
        @click="restore"
      />
      <span class="text-[13px] text-muted">{{ t('vhost.testedHint') }}</span>
    </template>
  </PanelSection>
  <UAlert v-if="isAdmin && view" color="neutral" variant="subtle" icon="i-cg-info" :description="t('vhost.managed')" />
  <UAlert v-if="refusal" color="error" variant="subtle" icon="i-cg-alert-circle" :title="t('vhost.refused')" :description="refusal" />
</template>
