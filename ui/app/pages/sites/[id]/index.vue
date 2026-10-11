<script setup lang="ts">
import type { Backup, CertificateStatus } from '~/types'
import type { SummaryRow } from '~/types/summary'

interface CacheStats { backend: string, hits: number, misses: number, entries: number, mem_used: number, mem_total: number, hit_rate_pct: number }

const { t, locale } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { run } = useAction()
const { site, staging, refresh } = useCurrentSite()

const { data: cert } = useLive(() => api.get<CertificateStatus>(`/api/sites/${site.value.id}/certificate`), { on: { types: ['cert.changed'], sites: () => site.value.id } })
const { data: backups } = useLive(() => api.get<Backup[]>(`/api/sites/${site.value.id}/backups`), { on: { types: ['backup.'], sites: () => site.value.id }, fallbackMs: 30000 })
// Object-cache counters come from the site's own PHP; only WordPress has them.
const { data: ocStats } = useLoad(() => isWordPress(site.value) && site.value.config.object_cache && site.value.status === 'active'
  ? api.get<CacheStats>(`/api/sites/${site.value.id}/cache-stats`).catch(() => null)
  : Promise.resolve(null))
const lastBackup = computed(() => backups.value?.[0] ?? null)

function ttl(sec: number) {
  if (!sec) return t('cg.site.ttlProfile')
  return sec % 3600 === 0 ? `${sec / 3600} h` : sec % 60 === 0 ? `${sec / 60} min` : `${sec} s`
}
const profile = computed(() => t(`site.profile.${site.value.profile}`).toLowerCase())

const stats = computed(() => {
  const s = site.value
  const out: { key: string, label: string, value: string, note: string, state?: { label: string, tone: 'ok' | 'warn' | 'err' | 'muted' } }[] = []
  out.push({
    key: 'cache',
    label: t('cg.site.stats.pageCache'),
    value: s.config.cache_enabled ? t('common.on').toLowerCase() : t('cg.site.stats.off'),
    note: s.config.cache_enabled ? t('cg.site.stats.pageCacheNote', { ttl: ttl(s.config.cache_ttl_sec), p: profile.value }) : '—'
  })
  if (isWordPress(s)) {
    const oc = ocStats.value
    out.push({
      key: 'oc',
      label: t('cg.site.stats.objectCache'),
      value: !s.config.object_cache ? t('cg.site.stats.off') : oc && oc.backend !== 'none' ? fmtPercent(oc.hit_rate_pct, 1) : '—',
      note: oc && oc.backend !== 'none' ? t('cg.site.stats.objectCacheNote', { entries: fmtNumber(oc.entries), mem: fmtBytes(oc.mem_used) }) : (s.config.object_cache ? 'APCu' : '—')
    })
  }
  const c = cert.value
  const days = c?.days_left ?? 0
  out.push({
    key: 'cert',
    label: t('cg.site.stats.certificate'),
    value: c?.exists ? t('cg.facts.days', { n: days }) : '—',
    note: c?.exists ? t('cg.site.stats.certNote', { issuer: c.issuer || '—', date: fmtDateTime(c.not_after, locale.value) }) : (c ? t('ssl.none') : '…'),
    state: c?.exists ? (days < 14 ? { label: t('cg.state.err'), tone: 'err' } : days < 30 ? { label: t('cg.state.warn'), tone: 'warn' } : { label: t('cg.state.ok'), tone: 'ok' }) : undefined
  })
  const b = lastBackup.value
  out.push({
    key: 'backup',
    label: t('cg.site.stats.lastBackup'),
    value: b ? fmtWhen(b.created_at, locale.value) : backups.value ? t('backup.never') : '…',
    note: b ? t('cg.site.stats.backupNote', { verify: t(`cg.facts.verify.${b.verify_status}`), est: b.restore_estimate_ms ? fmtDuration(b.restore_estimate_ms) : '—' }) : (s.config.backups.enabled ? '—' : t('cg.facts.backupOff')),
    state: b ? (b.verify_status === 'failed' ? { label: t('backup.verify.failed'), tone: 'err' } : b.verify_status === 'passed' ? { label: t('cg.state.ok'), tone: 'ok' } : { label: t('backup.verify.pending'), tone: 'muted' }) : undefined
  })
  return out
})

const purged = ref(false)
interface Action { key: string, label: string, icon: string, to?: string, onClick?: () => void, disabled?: boolean }
const actions = computed<Action[]>(() => {
  const s = site.value
  const off = !canManage.value || s.status !== 'active'
  const out: Action[] = [
    { key: 'purge', icon: 'i-cg-purge', label: purged.value ? t('cache.purgedAll') : t('cg.site.actions.purge'), disabled: off || !s.config.cache_enabled, onClick: () => void run(() => api.post(`/api/sites/${s.id}/purge`), t('cache.purgedAll')).then((ok) => { purged.value = ok }) },
    { key: 'backup', icon: 'i-cg-backup', label: t('cg.site.actions.backup'), disabled: off, onClick: () => void run(() => api.post(`/api/sites/${s.id}/backups`), t('backup.started')) }
  ]
  if (s.staging_of) out.push({ key: 'prod', icon: 'i-cg-arrow-left', label: t('cg.site.actions.openProduction'), to: `/sites/${s.staging_of}` })
  else if (staging.value) out.push({ key: 'push', icon: 'i-cg-upload', label: t('cg.site.actions.pushStaging'), to: `/sites/${s.id}/staging` })
  else out.push({ key: 'staging', icon: 'i-cg-staging', label: t('cg.site.actions.createStaging'), disabled: off, onClick: () => void run(() => api.post(`/api/sites/${s.id}/staging`), t('staging.cloneStarted')).then(refresh) })
  out.push({ key: 'warm', icon: 'i-cg-warm', label: t('cg.site.actions.warm'), disabled: off || !s.config.cache_enabled, onClick: () => void run(() => api.post(`/api/sites/${s.id}/warm`), t('wp.warming')) })
  return out
})

const details = computed(() => {
  const s = site.value
  const c = cert.value
  const rows: SummaryRow[] = [
    { k: t('cg.site.details.folder'), v: s.root_path, mono: true, copy: true },
    { k: t('cg.site.details.user'), v: `${s.system_user} · ${s.config.sftp_enabled ? t('cg.site.values.sftpOn') : t('cg.site.values.sftpOff')}` }
  ]
  if (s.aliases?.length) rows.push({ k: t('cg.site.details.aliases'), v: s.aliases.join(', ') })
  if (s.php_version) rows.push({ k: t('cg.site.details.php'), v: t('cg.site.values.phpLine', { v: s.php_version, m: s.config.php.memory_limit_mb || '—', u: s.config.php.upload_max_mb || '—' }) })
  if (s.type === 'proxy' && s.config.proxy_upstream) rows.push({ k: t('cg.site.details.upstream'), v: s.config.proxy_upstream })
  rows.push({ k: t('cg.site.details.database'), v: s.config.database.enabled ? `${s.config.database.name} · ${s.config.database.user}` : t('cg.site.values.dbOff'), mono: s.config.database.enabled })
  rows.push({ k: t('cg.site.details.cert'), v: c?.exists ? t('cg.site.values.certLine', { issuer: c.issuer || '—', n: c.days_left ?? 0 }) : t('ssl.none') })
  rows.push({ k: t('cg.site.details.backups'), v: s.config.backups.enabled ? t('cg.site.values.backupsLine', { s: s.config.backups.schedule || '—', d: s.config.backups.retention_days }) : t('cg.site.values.backupsOff') })
  rows.push({ k: t('cg.site.details.created'), v: fmtDateTime(s.created_at, locale.value) })
  return rows
})
</script>

<template>
  <section :aria-label="t('cg.site.state')" class="grid grid-cols-2 gap-3 lg:grid-cols-4">
    <StatCell v-for="m in stats" :key="m.key" :label="m.label" :value="m.value" :note="m.note" :state="m.state" />
  </section>

  <PanelSection :title="t('cg.site.actions.title')">
    <div class="flex flex-wrap gap-2">
      <UButton
        v-for="a in actions"
        :key="a.key"
        :to="a.to"
        :icon="a.icon"
        :label="a.label"
        :disabled="a.disabled"
        color="neutral"
        variant="outline"
        @click="a.onClick?.()"
      />
    </div>
  </PanelSection>

  <SiteUptime v-if="site.status === 'active'" />

  <div class="grid gap-6 lg:grid-cols-2">
    <PanelSection :title="t('overview.details')">
      <SummaryList :rows="details" />
    </PanelSection>
    <PanelSection :title="t('tasks.recent')" flush>
      <TaskFeed :site-id="site.id" />
    </PanelSection>
  </div>

  <SiteAddressCard />
</template>
