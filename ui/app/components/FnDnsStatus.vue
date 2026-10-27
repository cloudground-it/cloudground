<script setup lang="ts">
/**
 * Whether a function's names point at this server, one line each (the DNS
 * readiness check, internal/api SPEC §3.14), and the certificate that
 * covers them. Used by a function's Addresses tab and Settings → Functions.
 */
import type { CertState, DnsCheck } from '~/utils/functions'

defineProps<{ checks: DnsCheck[], certificate?: CertState | null, loading?: boolean }>()
defineEmits<{ again: [] }>()
const { t, locale } = useI18n()

const icon: Record<string, string> = { here: 'i-cg-check-circle', partial: 'i-cg-alert-triangle', proxied: 'i-cg-alert-triangle', elsewhere: 'i-cg-x-circle', unresolved: 'i-cg-x-circle', unknown: 'i-cg-help' }
const color: Record<string, string> = { here: 'text-cg-ok', partial: 'text-cg-warn', proxied: 'text-cg-warn', elsewhere: 'text-cg-err', unresolved: 'text-cg-err', unknown: 'text-muted' }
function text(c: DnsCheck): string {
  const others = (c.addresses ?? []).filter(a => !a.own).map(a => a.address ?? t('newSite.wizard.dns.private'))
  return t(`newSite.wizard.dns.${c.status}`, { addrs: [...new Set(others)].join(', '), server: (c.server ?? []).join(', ') })
}
</script>

<template>
  <section class="flex flex-col border border-default" :aria-label="t('fn.dns.title')">
    <div class="flex items-center justify-between gap-3 border-b border-default bg-muted px-4 py-2">
      <h3 class="m-0 text-[13px] font-semibold text-highlighted">{{ t('fn.dns.title') }}</h3>
      <UButton color="secondary" variant="link" size="sm" class="px-0" icon="i-cg-refresh" :label="t('newSite.wizard.dns.again')" :loading="loading" @click="$emit('again')" />
    </div>
    <ul class="m-0 list-none p-0" aria-live="polite">
      <li v-for="c in checks" :key="c.domain" class="grid grid-cols-[18px_minmax(0,1fr)] items-start gap-3 border-b border-default px-4 py-2.5 last:border-b-0" :data-dns="c.status">
        <UIcon :name="icon[c.status] ?? 'i-cg-help'" class="mt-0.5 size-[18px]" :class="color[c.status] ?? 'text-muted'" aria-hidden="true" />
        <span class="min-w-0">
          <span class="mono block text-[13px] text-highlighted [overflow-wrap:anywhere]">{{ c.domain }}</span>
          <span class="block text-[13px] text-muted">{{ text(c) }}</span>
        </span>
      </li>
      <li v-if="certificate" class="grid grid-cols-[18px_minmax(0,1fr)] items-start gap-3 px-4 py-2.5">
        <UIcon name="i-cg-certificate" class="mt-0.5 size-[18px]" :class="certificate.exists ? 'text-cg-ok' : 'text-muted'" aria-hidden="true" />
        <span class="text-[13px]" :class="certificate.exists ? 'text-highlighted' : 'text-muted'">
          {{ certificate.exists ? t('fn.dns.certificate', { issuer: certificate.issuer || '—', date: fmtDateTime(certificate.not_after, locale) }) : t('fn.dns.noCertificate') }}
        </span>
      </li>
    </ul>
  </section>
</template>
