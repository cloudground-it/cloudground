<script setup lang="ts">
import type { FileEntry } from '~/types'
import type { FileKind } from '~/utils/fileTypes'
import type { ConflictChoice } from '~/composables/useUploadQueue'

const { t, locale } = useI18n()
const api = useApi()
const toast = useToast()
const { canManage } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site } = useCurrentSite()

interface ReadResult { content: string, size: number, binary: boolean, too_large: boolean, encoding?: string }
interface Editing { path: string, name: string, content: string, encoding: string }
interface FileCard { path: string, name: string, size: number, reason: 'binary' | 'tooLarge', kind: FileKind, preview?: string }

const path = ref('')
const entries = ref<FileEntry[]>([])
const loading = ref(false)
const editing = ref<Editing | null>(null)
const dirty = ref(false)
const editor = ref<{ save: () => Promise<boolean>, saving: boolean } | null>(null)
const card = ref<FileCard | null>(null)
const upload = ref<HTMLInputElement | null>(null)
const prompt = ref<{ title: string, value: string, resolve: (v: string | null) => void } | null>(null)

const crumbs = computed(() => path.value.split('/').filter(Boolean))
const child = (name: string) => path.value ? `${path.value}/${name}` : name
const q = (p: string) => encodeURIComponent(p)

async function load(p: string) {
  loading.value = true
  try {
    const r = await api.get<{ entries: FileEntry[] | null }>(`/api/sites/${site.value.id}/files?path=${q(p)}`)
    entries.value = (r.entries ?? []).sort((a, b) => Number(b.is_dir) - Number(a.is_dir) || a.name.localeCompare(b.name))
    path.value = p
  } catch (e) {
    toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error' })
  } finally {
    loading.value = false
  }
}
void load('')

function ask(title: string, value = ''): Promise<string | null> {
  return new Promise(resolve => (prompt.value = { title, value, resolve }))
}
function answer(v: string | null) {
  prompt.value?.resolve(v?.trim() || null)
  prompt.value = null
}

async function open(e: FileEntry) {
  if (e.is_dir) return load(child(e.name))
  if (!canManage.value) return
  const p = child(e.name)
  const kind = detectFile(e.name)
  // A known binary type is never fetched as text: it goes straight to the
  // download card, as does anything over the editor's limit.
  if (kind.binary) return showCard({ path: p, name: e.name, size: e.size, reason: 'binary', kind })
  if (e.size > MAX_EDITABLE_BYTES) return showCard({ path: p, name: e.name, size: e.size, reason: 'tooLarge', kind })
  let res: ReadResult | null = null
  await run(async () => {
    res = await api.get<ReadResult>(`/api/sites/${site.value.id}/files/read?path=${q(p)}`)
  })
  const r = res as ReadResult | null
  if (!r) return
  if (r.binary) return showCard({ path: p, name: e.name, size: r.size, reason: 'binary', kind: { label: fileExt(e.name).toUpperCase() || '—', binary: 'binary', icon: 'i-cg-file' } })
  if (r.too_large) return showCard({ path: p, name: e.name, size: r.size, reason: 'tooLarge', kind })
  dirty.value = false
  editing.value = { path: p, name: e.name, content: r.content, encoding: r.encoding || 'utf-8' }
}

const PREVIEW_MAX = 5 * 1024 * 1024
async function showCard(c: FileCard) {
  card.value = c
  if (!c.kind.imageType || c.size > PREVIEW_MAX) return
  // The preview is a data: URL, which the panel's CSP allows (img-src
  // 'self' data:); a blob: URL it does not.
  try {
    const resp = await fetch(downloadURL(c.path), { credentials: 'same-origin' })
    if (!resp.ok) return
    const blob = new Blob([await resp.arrayBuffer()], { type: c.kind.imageType })
    const url = await new Promise<string>((resolve, reject) => {
      const fr = new FileReader()
      fr.onload = () => resolve(String(fr.result))
      fr.onerror = () => reject(fr.error)
      fr.readAsDataURL(blob)
    })
    if (card.value?.path === c.path) card.value = { ...card.value, preview: url }
  } catch {
    // No preview; the card still offers the download.
  }
}

function saveFile(content: string): Promise<boolean> {
  const ed = editing.value
  if (!ed) return Promise.resolve(false)
  return run(() => api.post(`/api/sites/${site.value.id}/files/write`, { path: ed.path, content }), t('common.saved'))
}
async function confirmDiscard(): Promise<boolean> {
  if (!editing.value || !dirty.value) return true
  return confirm({ title: t('editor.discardTitle'), description: t('editor.discardBody'), danger: true, confirmLabel: t('editor.discard') })
}
async function closeEditor() {
  if (!await confirmDiscard()) return
  editing.value = null
  dirty.value = false
}
useEventListener(window, 'beforeunload', (ev: BeforeUnloadEvent) => {
  if (editing.value && dirty.value) {
    ev.preventDefault()
    ev.returnValue = ''
  }
})
onBeforeRouteLeave(() => confirmDiscard())

// ---- uploads: the Upload button (several files) or a drop of files and folders.
const conflict = ref<{ name: string, folder: string, applyAll: boolean, resolve: (c: ConflictChoice) => void } | null>(null)
function answerConflict(choice: ConflictChoice) {
  const c = conflict.value
  if (!c) return
  conflict.value = null
  c.resolve(choice)
}
const queue = useUploadQueue({
  siteId: () => site.value.id,
  async listNames(dir) {
    if (dir === path.value) return new Map(entries.value.map(e => [e.name, e.is_dir]))
    try {
      const r = await api.get<{ entries: FileEntry[] | null }>(`/api/sites/${site.value.id}/files?path=${q(dir)}`)
      return new Map((r.entries ?? []).map(e => [e.name, e.is_dir]))
    } catch {
      return null
    }
  },
  async mkdir(dir) {
    await api.post(`/api/sites/${site.value.id}/files/manage`, { operation: 'mkdir', path: dir })
  },
  resolveConflict: (name, folder) => new Promise((resolve) => {
    const state = reactive({ name, folder, applyAll: false, resolve: (choice: ConflictChoice) => resolve({ choice, applyAll: state.applyAll }) })
    conflict.value = state
  }),
  onIdle: () => void load(path.value)
})
async function onUpload(ev: Event) {
  const input = ev.target as HTMLInputElement
  const picked = readPicked(input.files)
  input.value = ''
  await queue.enqueue(picked, path.value)
}

const dragDepth = ref(0)
const hasFiles = (e: DragEvent) => !!e.dataTransfer && Array.from(e.dataTransfer.types).includes('Files')
function onDragEnter(e: DragEvent) {
  if (!canManage.value || !hasFiles(e)) return
  e.preventDefault()
  dragDepth.value++
}
function onDragOver(e: DragEvent) {
  if (!canManage.value || !hasFiles(e)) return
  e.preventDefault()
  e.dataTransfer!.dropEffect = 'copy'
}
function onDragLeave() {
  if (dragDepth.value > 0) dragDepth.value--
}
async function onDrop(e: DragEvent) {
  dragDepth.value = 0
  if (!canManage.value || !hasFiles(e)) return
  e.preventDefault()
  const base = path.value
  // readDrop takes the entries now, while the event is live.
  const picked = readDrop(e.dataTransfer!)
  await queue.enqueue(await picked, base)
}
// A drop that misses the list must not make the browser open the file in
// place of the panel.
useEventListener(window, 'dragover', (e: DragEvent) => { if (hasFiles(e)) e.preventDefault() })
useEventListener(window, 'drop', (e: DragEvent) => {
  dragDepth.value = 0
  if (hasFiles(e)) e.preventDefault()
})
const dropTarget = computed(() => (path.value ? `/${path.value}` : site.value.domain))
const manage = (body: Record<string, string>, ok: string) => run(() => api.post(`/api/sites/${site.value.id}/files/manage`, body), ok).then(() => load(path.value))
async function mkdir() {
  const name = await ask(t('files.folderName'))
  if (name) await manage({ operation: 'mkdir', path: child(name) }, t('files.folderCreated'))
}
async function rename(e: FileEntry) {
  const name = await ask(t('files.newName'), e.name)
  if (name && name !== e.name) await manage({ operation: 'rename', path: child(e.name), dest: child(name) }, t('files.renamed'))
}
async function remove(e: FileEntry) {
  const ok = await confirm({ title: t('files.deleteTitle', { name: e.name }), description: e.is_dir ? t('files.deleteDir') : t('files.deleteFile'), danger: true, confirmLabel: t('common.delete') })
  if (ok) await manage({ operation: 'delete', path: child(e.name) }, t('files.deleted'))
}
const downloadURL = (p: string) => `/api/sites/${site.value.id}/files/download?path=${q(p)}`
const downloadPath = (p: string) => { window.location.href = downloadURL(p) }
const download = (e: FileEntry) => downloadPath(child(e.name))
const kindLabel = (k: FileKind) => k.binary ? `${t(`files.kind.${k.binary}`)} · ${k.label}` : k.label
</script>

<template>
  <div class="flex flex-col gap-6">
    <UploadQueue
      v-if="queue.items.value.length"
      :items="queue.items.value"
      :active="queue.active.value"
      :counts="queue.counts.value"
      @cancel="queue.cancel"
      @cancel-all="queue.cancelAll"
      @clear="queue.clear"
    />

    <PanelSection flush :label="t('tabs.files')">
      <template #toolbar>
        <div class="flex flex-wrap items-center gap-2">
          <UBreadcrumb :items="[{ label: site.domain, icon: 'i-cg-folder', onClick: () => load('') }, ...crumbs.map((c, i) => ({ label: c, onClick: () => load(crumbs.slice(0, i + 1).join('/')) }))]" class="min-w-0 basis-full sm:flex-1 sm:basis-auto" />
          <input ref="upload" type="file" multiple hidden @change="onUpload">
          <UButton color="neutral" variant="outline" icon="i-cg-folder-plus" :label="t('files.newFolder')" :disabled="!canManage || busy" @click="mkdir" />
          <UButton icon="i-cg-upload" :label="t('files.upload')" :disabled="!canManage || busy" @click="upload?.click()" />
        </div>
      </template>
      <div
        class="relative min-h-32"
        @dragenter="onDragEnter"
        @dragover="onDragOver"
        @dragleave="onDragLeave"
        @drop="onDrop"
      >
        <div
          v-if="dragDepth > 0"
          class="pointer-events-none absolute inset-0 z-10 flex flex-col items-center justify-center gap-2 border-2 border-dashed border-cg-blue bg-default/90 p-4 text-center"
        >
          <UIcon name="i-cg-upload" class="size-8 text-cg-blue" />
          <p class="text-sm font-medium text-highlighted">
            {{ t('upload.dropHere', { folder: dropTarget }) }}
          </p>
        </div>
        <button v-if="path" type="button" class="flex w-full items-center gap-3 px-4 py-2.5 text-left text-sm hover:bg-elevated sm:px-5" @click="load(crumbs.slice(0, -1).join('/'))">
          <UIcon name="i-cg-arrow-up" class="size-4 text-muted" /> ..
        </button>
        <div v-for="e in entries" :key="e.name" class="group flex items-center gap-3 border-t border-default px-4 py-2 text-sm first:border-t-0 hover:bg-elevated sm:px-5">
          <button type="button" class="flex min-w-0 flex-1 items-center gap-3 text-left" @click="open(e)">
            <UIcon :name="e.is_dir ? 'i-cg-folder' : detectFile(e.name).icon" class="size-4 shrink-0" :class="e.is_dir ? 'text-cg-blue' : 'text-muted'" />
            <span class="truncate" :class="e.is_dir ? 'font-medium text-highlighted' : ''">{{ e.name }}</span>
          </button>
          <span class="mono hidden text-xs text-dimmed md:block">{{ e.mode }}</span>
          <span class="mono hidden text-right text-xs whitespace-nowrap text-muted md:block">{{ fmtDateTime(e.mod_time, locale) }}</span>
          <span class="mono w-20 text-right text-xs text-muted">{{ e.is_dir ? '' : fmtBytes(e.size) }}</span>
          <div v-if="canManage" class="flex gap-0.5">
            <UButton v-if="!e.is_dir" size="xs" color="neutral" variant="ghost" square icon="i-cg-download" :aria-label="t('files.download')" @click="download(e)" />
            <span v-else class="inline-block min-w-7" aria-hidden="true" />
            <UButton size="xs" color="neutral" variant="ghost" square icon="i-cg-edit" :aria-label="t('files.rename')" @click="rename(e)" />
            <UButton size="xs" color="error" variant="ghost" square icon="i-cg-trash" :aria-label="t('common.delete')" @click="remove(e)" />
          </div>
        </div>
        <UEmpty v-if="!entries.length && !loading" size="sm" icon="i-cg-folder-open" :description="t('files.empty')" />
      </div>
      <template #footer>
        <p class="m-0 text-xs text-muted">
          {{ t('files.hint', { size: fmtBytes(UPLOAD_MAX_BYTES) }) }}
        </p>
      </template>
    </PanelSection>

    <UModal
      :open="!!editing"
      fullscreen
      :dismissible="false"
      :close="false"
      :ui="{ header: 'min-h-12 px-4 py-2 sm:px-4', body: 'flex min-h-0 flex-1 flex-col overflow-hidden p-0 sm:p-0' }"
      @update:open="v => { if (!v) closeEditor() }"
    >
      <template #header>
        <div v-if="editing" class="flex w-full min-w-0 items-center gap-2">
          <UIcon :name="detectFile(editing.name).icon" class="size-4 shrink-0 text-muted" />
          <p class="min-w-0 flex-1 truncate font-mono text-sm text-highlighted" :title="editing.path">
            {{ editing.path }}
          </p>
          <span v-if="dirty" class="flex shrink-0 items-center gap-1.5 text-xs text-warning" :title="t('editor.unsaved')">
            <span class="size-2 bg-warning" />
            <span class="hidden sm:inline">{{ t('editor.unsaved') }}</span>
          </span>
          <UButton size="sm" color="neutral" variant="ghost" square icon="i-cg-download" :aria-label="t('files.download')" @click="downloadPath(editing.path)" />
          <UButton color="primary" variant="solid" size="sm" icon="i-cg-check" :label="t('common.save')" :loading="editor?.saving" :disabled="!dirty" @click="editor?.save()" />
          <UButton size="sm" color="neutral" variant="ghost" square icon="i-cg-x" :aria-label="t('editor.close')" @click="closeEditor" />
        </div>
      </template>
      <template #body>
        <LazyCodeEditor
          v-if="editing"
          :key="editing.path"
          ref="editor"
          v-model:dirty="dirty"
          :content="editing.content"
          :filename="editing.name"
          :encoding="editing.encoding"
          :save="saveFile"
        />
      </template>
    </UModal>

    <UModal :open="!!card" :title="card?.name" @update:open="v => { if (!v) card = null }">
      <template #body>
        <div v-if="card" class="space-y-4">
          <img v-if="card.preview" :src="card.preview" :alt="card.name" class="mx-auto max-h-64 max-w-full rounded-md border border-default bg-muted object-contain">
          <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
            <dt class="text-muted">
              {{ t('files.type') }}
            </dt>
            <dd class="min-w-0 truncate">
              {{ kindLabel(card.kind) }}
            </dd>
            <dt class="text-muted">
              {{ t('files.size') }}
            </dt>
            <dd>{{ fmtBytes(card.size) }}</dd>
          </dl>
          <p class="text-sm text-muted">
            {{ card.reason === 'binary' ? t('files.binaryHint') : t('files.tooLargeHint', { size: fmtBytes(MAX_EDITABLE_BYTES) }) }}
          </p>
        </div>
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton color="neutral" variant="ghost" :label="t('editor.close')" @click="card = null" />
          <UButton color="primary" variant="solid" icon="i-cg-download" :label="t('files.download')" @click="card && downloadPath(card.path)" />
        </div>
      </template>
    </UModal>

    <UModal :open="!!conflict" :title="t('upload.conflictTitle')" :dismissible="false" :close="false">
      <template #body>
        <div v-if="conflict" class="space-y-4">
          <p class="text-sm">
            {{ t('upload.conflictBody', { name: conflict.name, folder: conflict.folder }) }}
          </p>
          <UCheckbox v-model="conflict.applyAll" :label="t('upload.applyAll')" />
        </div>
      </template>
      <template #footer>
        <div class="flex w-full flex-wrap justify-end gap-2">
          <UButton color="neutral" variant="ghost" :label="t('upload.cancelBatch')" @click="answerConflict('cancel')" />
          <UButton color="neutral" variant="outline" :label="t('upload.skip')" @click="answerConflict('skip')" />
          <UButton color="warning" :label="t('upload.overwrite')" @click="answerConflict('overwrite')" />
        </div>
      </template>
    </UModal>

    <UModal :open="!!prompt" :title="prompt?.title" @update:open="v => { if (!v) answer(null) }">
      <template #body>
        <UInput v-if="prompt" v-model="prompt.value" class="w-full" autofocus @keydown.enter="answer(prompt.value)" />
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="answer(null)" />
          <UButton color="primary" variant="solid" :label="t('common.confirm')" @click="answer(prompt?.value ?? null)" />
        </div>
      </template>
    </UModal>
  </div>
</template>
