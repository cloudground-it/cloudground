/**
 * The file manager's upload queue: files and folders dropped on the list or
 * picked with the Upload button go to POST /api/sites/{id}/files/upload, a
 * few at a time, each with its own progress, error and cancel.
 *
 * The endpoint takes one raw file per request (application/octet-stream,
 * the destination in ?path=) and at most UPLOAD_MAX_BYTES. A folder is
 * recreated with the files/manage mkdir operation first, since the agent
 * writes a file only into a directory that exists.
 */

/** The API's limit per upload: maxPanelTransfer in internal/api/site_files.go. */
export const UPLOAD_MAX_BYTES = 16 * 1024 * 1024
const CONCURRENCY = 3

export type UploadStatus = 'queued' | 'uploading' | 'done' | 'failed' | 'canceled' | 'skipped'

export interface UploadItem {
  id: number
  /** Path shown to the operator, relative to the folder it was dropped in. */
  name: string
  /** Destination, relative to the site root. */
  dest: string
  file: File | null
  size: number
  loaded: number
  status: UploadStatus
  error?: string
}

/** A file to upload, by its path relative to the drop target. */
export interface PickedFile { rel: string, file: File }
/** What was dropped or picked: files, and folders to create (also empty ones). */
export interface Picked { files: PickedFile[], dirs: string[], errors: string[] }

export type ConflictChoice = 'overwrite' | 'skip' | 'cancel'

interface QueueOptions {
  siteId: () => number
  /** Names in a folder (relative to the site root), or null if it does not exist. */
  listNames: (dir: string) => Promise<Map<string, boolean> | null>
  mkdir: (dir: string) => Promise<void>
  /** Asks about one name conflict; applyAll repeats the answer for the rest of the batch. */
  resolveConflict: (name: string, folder: string) => Promise<{ choice: ConflictChoice, applyAll: boolean }>
  /** Runs when the queue has drained. */
  onIdle: () => void
}

const join = (a: string, b: string) => (a ? `${a}/${b}` : b)
const parentOf = (p: string) => (p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : '')
const baseOf = (p: string) => p.slice(p.lastIndexOf('/') + 1)

/** Reads a drop's files and folders. Call it synchronously from the drop handler. */
export function readDrop(dt: DataTransfer): Promise<Picked> {
  // Entries must be taken while the event is being dispatched: the list is
  // emptied as soon as the handler returns.
  const roots: (FileSystemEntry | File)[] = []
  for (const item of Array.from(dt.items)) {
    if (item.kind !== 'file') continue
    const entry = item.webkitGetAsEntry?.()
    if (entry) roots.push(entry)
    else {
      const f = item.getAsFile()
      if (f) roots.push(f)
    }
  }
  if (!roots.length) for (const f of Array.from(dt.files)) roots.push(f)
  return walk(roots)
}

/** Files from an <input type="file" multiple>. */
export function readPicked(list: FileList | null): Picked {
  return { files: Array.from(list ?? []).map(file => ({ rel: file.name, file })), dirs: [], errors: [] }
}

async function walk(roots: (FileSystemEntry | File)[]): Promise<Picked> {
  const out: Picked = { files: [], dirs: [], errors: [] }
  const fileOf = (e: FileSystemFileEntry) => new Promise<File>((resolve, reject) => e.file(resolve, reject))
  const readAll = async (d: FileSystemDirectoryEntry) => {
    const reader = d.createReader()
    const all: FileSystemEntry[] = []
    // readEntries returns the listing in batches (100 in Chrome) until empty.
    for (;;) {
      const batch = await new Promise<FileSystemEntry[]>((resolve, reject) => reader.readEntries(resolve, reject))
      if (!batch.length) return all
      all.push(...batch)
    }
  }
  const visit = async (e: FileSystemEntry, rel: string) => {
    if (!safeName(e.name)) {
      out.errors.push(rel)
      return
    }
    try {
      if (e.isFile) out.files.push({ rel, file: await fileOf(e as FileSystemFileEntry) })
      else if (e.isDirectory) {
        out.dirs.push(rel)
        for (const c of await readAll(e as FileSystemDirectoryEntry)) await visit(c, `${rel}/${c.name}`)
      }
    } catch {
      out.errors.push(rel)
    }
  }
  for (const r of roots) {
    if (r instanceof File) out.files.push({ rel: r.name, file: r })
    else await visit(r, r.name)
  }
  return out
}

const safeName = (n: string) => !!n && n !== '.' && n !== '..' && !n.includes('/') && !n.includes('\0')

export function useUploadQueue(opts: QueueOptions) {
  const { t } = useI18n()
  const items = ref<UploadItem[]>([])
  const xhrs = new Map<number, XMLHttpRequest>()
  let seq = 0
  let running = 0

  const active = computed(() => items.value.some(i => i.status === 'queued' || i.status === 'uploading'))
  const counts = computed(() => {
    const c = { total: items.value.length, done: 0, failed: 0, finished: 0 }
    for (const i of items.value) {
      if (i.status === 'done') c.done++
      if (i.status === 'failed') c.failed++
      if (i.status !== 'queued' && i.status !== 'uploading') c.finished++
    }
    return c
  })

  const item = (id: number) => items.value.find(i => i.id === id)

  /** Queues what was dropped or picked into folder base (relative to the site root). */
  async function enqueue(picked: Picked, base: string) {
    // The batch joins the queue only once every conflict is answered, so
    // nothing of it starts while the operator is still choosing.
    const batch: UploadItem[] = []
    const add = (name: string, dest: string, file: File | null, status: UploadStatus, error?: string) => {
      batch.push({ id: ++seq, name, dest, file, size: file?.size ?? 0, loaded: 0, status, error })
    }
    for (const e of picked.errors) add(e, join(base, e), null, 'failed', t('upload.unreadable'))
    if (!picked.files.length && !picked.dirs.length) {
      items.value.push(...batch)
      return
    }

    // What exists already, folder by folder, so a conflict is asked about
    // before anything is sent. A folder that does not exist has no conflicts.
    const listings = new Map<string, Map<string, boolean> | null>()
    // Every destination folder is under base, so the recursion ends there.
    const names = async (dir: string): Promise<Map<string, boolean> | null> => {
      if (!listings.has(dir)) {
        if (dir === base) listings.set(dir, await opts.listNames(dir))
        else {
          // Only a folder its parent lists as a folder is fetched.
          const parent = await names(parentOf(dir))
          listings.set(dir, parent?.get(baseOf(dir)) === true ? await opts.listNames(dir) : null)
        }
      }
      return listings.get(dir) ?? null
    }

    // Each file first: too large, or a name that exists (asked about once
    // per conflict). Nothing is created or sent until this is settled.
    let always: ConflictChoice | null = null
    let stopped = false
    const accepted: { f: PickedFile, dest: string }[] = []
    for (const f of picked.files) {
      const dest = join(base, f.rel)
      if (stopped) {
        add(f.rel, dest, f.file, 'canceled')
        continue
      }
      if (f.file.size > UPLOAD_MAX_BYTES) {
        add(f.rel, dest, f.file, 'failed', t('upload.tooLarge', { size: fmtBytes(UPLOAD_MAX_BYTES) }))
        continue
      }
      const existing = (await names(parentOf(dest)))?.get(baseOf(dest))
      if (existing === true) {
        add(f.rel, dest, f.file, 'failed', t('upload.folderInTheWay'))
        continue
      }
      if (existing === false) {
        let choice: ConflictChoice | null = always
        if (!choice) {
          const r = await opts.resolveConflict(f.rel, base || '/')
          choice = r.choice
          if (r.applyAll && choice !== 'cancel') always = choice
        }
        if (choice === 'cancel') {
          // Cancel stops the whole batch.
          stopped = true
          for (const a of accepted) add(a.f.rel, a.dest, a.f.file, 'canceled')
          accepted.length = 0
          add(f.rel, dest, f.file, 'canceled')
          continue
        }
        if (choice === 'skip') {
          add(f.rel, dest, f.file, 'skipped', t('upload.exists'))
          continue
        }
      }
      accepted.push({ f, dest })
    }

    // Then the folders, parents first. One that cannot be created fails the
    // files in it, and the folders under it.
    const dirs = new Set<string>()
    if (!stopped) {
      for (const rel of picked.dirs.concat(accepted.map(a => parentOf(a.f.rel)))) {
        for (let d = rel; d; d = parentOf(d)) dirs.add(d)
      }
    }
    const failedDirs = new Set<string>()
    for (const d of [...dirs].sort((x, y) => x.split('/').length - y.split('/').length)) {
      const dest = join(base, d)
      if (failedDirs.has(parentOf(d))) {
        failedDirs.add(d)
        continue
      }
      const existing = (await names(parentOf(dest)))?.get(baseOf(dest))
      if (existing === true) continue
      if (existing === false) {
        failedDirs.add(d)
        add(d + '/', dest, null, 'failed', t('upload.fileInTheWay'))
        continue
      }
      try {
        await opts.mkdir(dest)
        listings.set(dest, new Map())
        listings.get(parentOf(dest))?.set(baseOf(dest), true)
      } catch (e) {
        failedDirs.add(d)
        add(d + '/', dest, null, 'failed', errorMessage(e))
      }
    }
    for (const { f, dest } of accepted) {
      if (failedDirs.has(parentOf(f.rel))) add(f.rel, dest, f.file, 'failed', t('upload.folderFailed'))
      else add(f.rel, dest, f.file, 'queued')
    }
    items.value.push(...batch)
    pump()
    if (!active.value) opts.onIdle()
  }

  function pump() {
    while (running < CONCURRENCY) {
      const next = items.value.find(i => i.status === 'queued')
      if (!next) break
      start(next)
    }
  }

  function start(it: UploadItem) {
    const id = it.id
    const xhr = new XMLHttpRequest()
    xhrs.set(id, xhr)
    running++
    it.status = 'uploading'
    const finish = (status: UploadStatus, error?: string) => {
      const cur = item(id)
      if (cur && cur.status === 'uploading') {
        cur.status = status
        cur.error = error
        if (status === 'done') cur.loaded = cur.size
        cur.file = null
      }
      xhrs.delete(id)
      running--
      pump()
      if (!active.value) opts.onIdle()
    }
    xhr.upload.onprogress = (e) => {
      const cur = item(id)
      if (cur && e.lengthComputable) cur.loaded = e.loaded
    }
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) return finish('done')
      let msg = ''
      try {
        msg = JSON.parse(xhr.responseText).error ?? ''
      } catch {
        // Not the API's JSON: a proxy refused it (nginx's 413 page, say).
      }
      if (!msg && xhr.status === 413) msg = t('upload.refusedTooLarge')
      finish('failed', msg || `HTTP ${xhr.status}`)
    }
    xhr.onerror = () => finish('failed', t('upload.networkError'))
    xhr.onabort = () => finish('canceled')
    xhr.open('POST', `/api/sites/${opts.siteId()}/files/upload?path=${encodeURIComponent(it.dest)}`)
    xhr.setRequestHeader('Content-Type', 'application/octet-stream')
    xhr.setRequestHeader('Accept', 'application/json')
    xhr.send(it.file!)
  }

  function cancel(id: number) {
    const it = item(id)
    if (!it) return
    if (it.status === 'uploading') xhrs.get(id)?.abort()
    else if (it.status === 'queued') {
      it.status = 'canceled'
      it.file = null
      if (!active.value) opts.onIdle()
    }
  }
  function cancelAll() {
    for (const it of items.value) if (it.status === 'queued') it.status = 'canceled'
    for (const x of xhrs.values()) x.abort()
  }
  /** Drops the finished rows; the panel closes when nothing is left. */
  function clear() {
    items.value = items.value.filter(i => i.status === 'queued' || i.status === 'uploading')
  }

  return { items, active, counts, enqueue, cancel, cancelAll, clear }
}
