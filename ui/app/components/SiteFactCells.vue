<script setup lang="ts">
/** The certificate or last-backup line of a site row, in words. */
import type { Site } from '~/types'
import type { SiteFacts } from '~/composables/useSiteFacts'

const props = defineProps<{ site: Site, facts?: SiteFacts, show: 'cert' | 'backup' }>()
const { t, locale } = useI18n()

const cert = computed(() => {
  const c = props.facts?.cert
  if (c === undefined) return { text: '…', tone: 'text-dimmed' }
  if (!c || !c.exists) return { text: t('cg.facts.noCert'), tone: 'text-cg-warn' }
  const issuer = /let.?s encrypt/i.test(c.issuer ?? '') ? 'Let’s Encrypt' : (c.issuer || '—')
  const days = c.days_left ?? 0
  return { text: `${issuer} · ${t('cg.facts.days', { n: days })}`, tone: days < 14 ? 'text-cg-err' : days < 30 ? 'text-cg-warn' : 'text-muted' }
})
const backup = computed(() => {
  if (!props.site.config.backups?.enabled && !props.facts?.backup) return { text: t('cg.facts.backupOff'), tone: 'text-muted' }
  const b = props.facts?.backup
  if (b === undefined) return { text: '…', tone: 'text-dimmed' }
  if (!b) return { text: t('cg.facts.backupNone'), tone: 'text-cg-warn' }
  const when = fmtWhen(b.created_at, locale.value)
  return {
    text: `${when} · ${t(`cg.facts.verify.${b.verify_status}`)}`,
    tone: b.verify_status === 'failed' ? 'text-cg-err' : 'text-muted'
  }
})
const v = computed(() => props.show === 'cert' ? cert.value : backup.value)
</script>

<template>
  <span class="mono text-[13px]" :class="v.tone">{{ v.text }}</span>
</template>
