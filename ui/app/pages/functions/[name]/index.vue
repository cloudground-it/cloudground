<script setup lang="ts">
/**
 * A function's code: the files of its live release in an editor, a file
 * added or removed, the whole set deployed as a new release (PUT
 * /api/functions/{name} {files}; a dependency step that failed is a 400 with
 * its output), and a test run of the deployed code (POST …/invoke).
 */
import type { FnInvokeResult } from '~/utils/functions'

const { t } = useI18n()
const api = useApi()
const toast = useToast()
const { isAdmin } = useAuth()
const { fn, base, refresh } = useCurrentFunction()

const entry = computed(() => fn.value.entry ?? fnEntry(fn.value.runtime))
const live = computed<Record<string, string>>(() => fn.value.files ?? {})
// The files as edited: the editor writes into this when the file shown changes.
const drafts = ref<Record<string, string>>({})
const selected = ref('')
function reset() {
  drafts.value = { ...live.value }
  if (!(entry.value in drafts.value)) drafts.value[entry.value] = ''
  if (!(selected.value in drafts.value)) selected.value = entry.value
}
watch(live, reset, { immediate: true })

const editor = ref<{ text: () => string } | null>(null)
const editorDirty = ref(false)
function capture() {
  if (editor.value && selected.value in drafts.value) drafts.value[selected.value] = editor.value.text()
}
function show(path: string) {
  capture()
  selected.value = path
}
const paths = computed(() => fnFileOrder(Object.keys(drafts.value), entry.value))
const changedPaths = computed(() => {
  const all = new Set([...Object.keys(live.value), ...Object.keys(drafts.value)])
  return [...all].filter(p => live.value[p] !== drafts.value[p])
})
const dirty = computed(() => changedPaths.value.length > 0 || editorDirty.value)
const isChanged = (p: string) => changedPaths.value.includes(p) || (p === selected.value && editorDirty.value)

// ---- adding and removing files ----
const newPath = ref('')
const pathError = computed(() => {
  const p = newPath.value.trim()
  if (!p) return undefined
  if (!fnPathOK(p)) return t('fn.code.pathRule')
  if (p in drafts.value) return t('fn.code.pathExists', { path: p })
  return undefined
})
function addFile() {
  const p = newPath.value.trim()
  if (!p || pathError.value) return
  capture()
  drafts.value = { ...drafts.value, [p]: '' }
  selected.value = p
  newPath.value = ''
}
function removeFile(p: string) {
  if (p === entry.value) return
  capture()
  const next = { ...drafts.value }
  delete next[p]
  drafts.value = next
  if (selected.value === p) selected.value = entry.value
}

// ---- deploying ----
const deploying = ref(false)
const deployError = ref('')
async function deploy(): Promise<boolean> {
  capture()
  deploying.value = true
  deployError.value = ''
  try {
    await api.put(base.value, { files: drafts.value })
    toast.add({ title: t('fn.code.deployed'), color: 'success', icon: 'i-cg-check-circle' })
    await refresh()
    editorDirty.value = false
    return true
  } catch (e) {
    deployError.value = errorMessage(e)
    return false
  } finally {
    deploying.value = false
  }
}
// Ctrl/⌘+S in the editor deploys, like the button.
const saveFromEditor = async (content: string) => {
  drafts.value[selected.value] = content
  return dirty.value ? deploy() : true
}
function discard() {
  reset()
  editorDirty.value = false
  deployError.value = ''
  editorKey.value++
}
const editorKey = ref(0)

// ---- a test run ----
const test = reactive({ method: 'POST', path: '/', body: '' })
const methods = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE'].map(m => ({ label: m, value: m }))
const result = ref<FnInvokeResult | null>(null)
const testing = ref(false)
async function invoke() {
  testing.value = true
  try {
    result.value = await api.post<FnInvokeResult>(`${base.value}/invoke`, { method: test.method, path: test.path || '/', body: test.body })
  } catch (e) {
    toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
  } finally {
    testing.value = false
  }
}
const prettyBody = computed(() => {
  const b = result.value?.body ?? ''
  try {
    return JSON.stringify(JSON.parse(b), null, 2)
  } catch {
    return b
  }
})
</script>

<template>
  <div class="flex flex-col gap-6">
    <PanelSection :title="t('fn.code.title')" :description="t(`fn.code.hint.${fn.runtime}`)" flush>
      <template #actions>
        <span v-if="dirty" class="flex items-center gap-1.5 text-[13px] text-cg-warn">
          <span class="size-2 rounded-full bg-current" aria-hidden="true" />{{ t('fn.code.unsaved') }}
        </span>
      </template>
      <div class="grid min-w-0 lg:grid-cols-[15rem_minmax(0,1fr)]">
        <nav :aria-label="t('fn.code.files')" class="flex flex-col border-b border-default bg-muted lg:border-e lg:border-b-0">
          <h3 class="m-0 px-4 pt-3 pb-1.5 text-[12px] font-semibold tracking-wide text-muted uppercase">{{ t('fn.code.files') }}</h3>
          <ul class="m-0 flex list-none flex-col p-0 pb-2">
            <li v-for="p in paths" :key="p" class="group flex items-center">
              <button
                type="button"
                class="flex min-w-0 flex-1 items-center gap-2 px-4 py-1.5 text-left text-[13px]"
                :class="selected === p ? 'bg-default font-medium text-highlighted shadow-[inset_2px_0_0_var(--ui-primary)]' : 'text-toned hover:bg-elevated'"
                :aria-current="selected === p ? 'true' : undefined"
                @click="show(p)"
              >
                <UIcon name="i-cg-file-code" class="size-4 shrink-0 text-muted" aria-hidden="true" />
                <span class="mono min-w-0 flex-1 truncate">{{ p }}</span>
                <UBadge v-if="p === entry" size="sm" color="neutral" variant="outline" :label="t('fn.code.entry')" />
                <span v-if="isChanged(p)" class="size-1.5 shrink-0 rounded-full bg-cg-warn" :title="t('fn.code.unsaved')" />
              </button>
              <UButton
                v-if="isAdmin && p !== entry"
                size="xs"
                color="neutral"
                variant="ghost"
                square
                icon="i-cg-x"
                class="me-1 opacity-60 group-hover:opacity-100"
                :aria-label="t('fn.code.remove', { path: p })"
                @click="removeFile(p)"
              />
            </li>
          </ul>
          <form v-if="isAdmin" class="mt-auto flex flex-col gap-1 border-t border-default p-3" @submit.prevent="addFile">
            <UFieldGroup class="w-full">
              <UInput v-model="newPath" size="sm" :placeholder="fn.runtime === 'node' ? 'lib/util.js' : 'utils.py'" :aria-label="t('fn.code.addFile')" class="min-w-0 flex-1" :ui="{ base: 'font-mono' }" autocomplete="off" spellcheck="false" />
              <UButton type="submit" size="sm" color="neutral" variant="outline" icon="i-cg-plus" :aria-label="t('fn.code.addFile')" :disabled="!newPath.trim() || !!pathError" />
            </UFieldGroup>
            <span v-if="pathError" class="text-xs text-error">{{ pathError }}</span>
            <span v-else class="text-xs text-muted">{{ t(`fn.code.deps.${fn.runtime}`) }}</span>
          </form>
        </nav>
        <div class="h-[30rem] min-w-0">
          <LazyCodeEditor
            :key="`${selected}:${fn.release}:${editorKey}`"
            ref="editor"
            v-model:dirty="editorDirty"
            :content="drafts[selected] ?? ''"
            :filename="selected"
            encoding="utf-8"
            :readonly="!isAdmin"
            :save="saveFromEditor"
          />
        </div>
      </div>
      <UAlert v-if="deployError" color="error" variant="subtle" icon="i-cg-alert-circle" :title="t('fn.code.deployFailed')" class="rounded-none border-t border-default">
        <template #description>
          <pre class="m-0 max-h-72 overflow-auto font-mono text-xs whitespace-pre-wrap">{{ deployError }}</pre>
        </template>
      </UAlert>
      <template v-if="isAdmin" #footer>
        <UButton icon="i-cg-deploy" :label="t('fn.code.deploy')" :loading="deploying" :disabled="!dirty" @click="deploy" />
        <UButton v-if="dirty" color="neutral" variant="ghost" :label="t('fn.code.discard')" :disabled="deploying" @click="discard" />
        <span class="text-[13px] text-muted">{{ t('fn.code.deployHint') }}</span>
      </template>
    </PanelSection>

    <PanelSection :title="t('fn.test.title')" :description="t('fn.test.hint')" icon="i-cg-play">
      <div class="grid gap-4 sm:grid-cols-[8rem_minmax(0,1fr)]">
        <UFormField :label="t('fn.test.method')">
          <USelect v-model="test.method" :items="methods" class="w-full" :disabled="!isAdmin" />
        </UFormField>
        <UFormField :label="t('fn.test.path')">
          <UInput v-model="test.path" placeholder="/" class="w-full font-mono" :disabled="!isAdmin" />
        </UFormField>
      </div>
      <UFormField :label="t('fn.test.body')" :hint="t('common.optional')">
        <UTextarea v-model="test.body" :rows="4" class="w-full" :ui="{ base: 'font-mono text-xs' }" placeholder="{&quot;hello&quot;: &quot;world&quot;}" spellcheck="false" :disabled="!isAdmin" />
      </UFormField>
      <div v-if="result" class="flex flex-col border border-default" data-testid="fn-test-result">
        <div class="flex flex-wrap items-center gap-3 border-b border-default bg-muted px-4 py-2 text-[13px]">
          <UBadge :color="httpStatusColor(result.status)" variant="subtle" class="mono" :label="result.status ? `HTTP ${result.status}` : t('fn.test.noAnswer')" />
          <span class="mono text-muted">{{ result.ms }} ms</span>
        </div>
        <pre class="m-0 max-h-80 overflow-auto px-4 py-3 font-mono text-xs whitespace-pre-wrap" :class="result.error ? 'text-error' : 'text-highlighted'">{{ result.error || prettyBody || t('fn.test.empty') }}</pre>
      </div>
      <template v-if="isAdmin" #footer>
        <UButton color="neutral" variant="outline" icon="i-cg-play" :label="t('fn.test.run')" :loading="testing" @click="invoke" />
        <span v-if="dirty" class="text-[13px] text-cg-warn">{{ t('fn.test.notDeployed') }}</span>
      </template>
    </PanelSection>
  </div>
</template>
