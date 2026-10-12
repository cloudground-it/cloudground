<script setup lang="ts">
import type { Alert } from '~/types'

const props = withDefaults(defineProps<{ alerts: Alert[], limit?: number, inset?: boolean }>(), { limit: 0, inset: false })
const emit = defineEmits<{ dismissed: [id: string] }>()
const { t, locale } = useI18n()
const api = useApi()
const toast = useToast()
const { canManage } = useAuth()
const { label } = useTaskNames()
const open = ref<Set<string>>(new Set())
const shown = computed(() => props.limit ? props.alerts.slice(0, props.limit) : props.alerts)


// A task alert shows the failed job, named in the interface's language, as
// the title and its error, which can be long and technical, folded. The
// panel sends the job's name (task_name, from the catalogue the task list
// and the webhook share) and its error (detail); an older one only the
// English "<kind words> failed: <error>", parsed as a fallback.
function parts(a: Alert): { title: string, summary: string, detail: string } {
  let title = t(`alerts.kind.${a.kind}`, a.kind)
  let detail = a.text || a.message
  if (a.kind === 'task' && a.task_kind) {
    title = t('alerts.failed', { kind: a.task_name || label(a.task_kind) })
    detail = (a.detail ?? '').replace(/^agent: /, '')
  } else if (a.kind === 'task') {
    const m = a.message.match(/^(.+?) failed: ([\s\S]*)$/)
    if (m) {
      const kind = m[1]!.trim().replace(/ /g, '.')
      title = t('alerts.failed', { kind: label(kind) })
      detail = m[2]!.replace(/^agent: /, '')
    }
  }
  const summary = detail.split('\n')[0]!.slice(0, 160)
  return { title, summary, detail }
}

function toggle(id: string) {
  const next = new Set(open.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  open.value = next
}

async function dismiss(a: Alert) {
  try {
    await api.post(`/api/system/alerts/${a.id}/dismiss`)
    emit('dismissed', a.id)
    toast.add({ title: t('alerts.dismissed'), color: 'neutral', icon: 'i-cg-archive' })
  } catch (e) {
    toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error' })
  }
}
</script>


<template>
  <p v-if="!alerts.length" class="m-0 flex items-center gap-3 py-3.5 text-sm text-muted" :class="inset ? 'px-4 sm:px-5' : ''">
    <UBadge color="success" variant="subtle" :label="t('cg.level.ok')" />
    {{ t('alerts.none') }}
  </p>
  <ul v-else class="m-0 list-none divide-y divide-default p-0">
    <li v-for="a in shown" :key="a.id" class="flex gap-3.5 py-3.5" :class="inset ? 'px-4 sm:px-5' : ''">
      <UBadge :color="a.level === 'error' ? 'error' : 'warning'" variant="subtle" :label="t(`cg.level.${a.level}`)" class="mt-1 w-20 shrink-0 self-start whitespace-normal sm:w-24" />
      <div class="min-w-0 flex-1">
        <p class="m-0 text-[15px] font-semibold text-highlighted">
          {{ parts(a).title }}
        </p>
        <p class="m-0 mt-0.5 text-[13px] text-muted">
          {{ a.site || t('alerts.server') }}<template v-if="a.at">
            · {{ fmtAgo(a.at, locale) }}
          </template>
        </p>
        <p v-if="!open.has(a.id)" class="m-0 mt-1 line-clamp-2 text-[13px] break-words text-muted">
          {{ parts(a).summary }}
        </p>
        <pre v-else class="mono mt-2 max-h-60 overflow-auto bg-cg-code-bg p-3 text-xs break-all whitespace-pre-wrap text-cg-code-fg">{{ parts(a).detail }}</pre>
        <div class="mt-1.5 flex flex-wrap gap-x-4 gap-y-1">
          <UButton v-if="parts(a).detail.length > parts(a).summary.length" variant="link" size="xs" :label="open.has(a.id) ? t('alerts.hideDetails') : t('alerts.details')" @click="toggle(a.id)" />
          <UButton v-if="a.site_id" variant="link" size="xs" :to="`/sites/${a.site_id}`" :label="t('alerts.open')" />
          <UButton v-if="canManage" variant="link" size="xs" :label="t('alerts.dismiss')" @click="dismiss(a)" />
        </div>
      </div>
    </li>
    <li v-if="shown.length < alerts.length" class="py-3 text-[13px] text-muted" :class="inset ? 'px-4 sm:px-5' : ''">
      {{ t('alerts.more', { n: alerts.length - shown.length }) }}
    </li>
  </ul>
</template>
