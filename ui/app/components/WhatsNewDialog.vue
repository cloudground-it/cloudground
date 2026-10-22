<script setup lang="ts">
/**
 * The first time an administrator opens the panel after an update, the new
 * release's changelog entry (and those of any release skipped between), once
 * per account (issue 106). Closing it in any way counts as seen.
 */
const { t } = useI18n()
const { isAdmin, user, enrolRequired } = useAuth()
const { log, load, markSeen } = useChangelog()
const toast = useToast()

const open = ref(false)
const shown = computed(() => (log.value?.entries ?? []).filter(e => log.value!.unseen.includes(e.version)))

onMounted(async () => {
  if (!user.value || !isAdmin.value || enrolRequired.value) return
  const l = await load()
  if (l?.unseen.length) open.value = true
})

async function dismiss() {
  open.value = false
  try {
    await markSeen()
  } catch (e) {
    toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error' })
  }
}
</script>

<template>
  <UModal
    :open="open"
    :title="t('whatsnew.dialogTitle', { v: shown[0]?.version ?? '' })"
    :description="shown.length > 1 ? t('whatsnew.skipped', shown.length) : t('whatsnew.dialogHint')"
    :ui="{ content: 'max-w-2xl' }"
    @update:open="(v: boolean) => { if (!v) void dismiss() }"
  >
    <template #body>
      <div class="flex flex-col gap-6">
        <section v-for="e in shown" :key="e.version" :aria-label="e.version" class="flex flex-col gap-2">
          <h3 v-if="shown.length > 1" class="mono m-0 text-sm font-semibold text-highlighted">
            {{ e.version }}
          </h3>
          <MarkdownText :source="e.body" />
        </section>
      </div>
    </template>
    <template #footer>
      <div class="flex w-full flex-wrap items-center justify-between gap-2">
        <UButton color="neutral" variant="link" :label="t('whatsnew.all')" to="/whats-new" @click="dismiss" />
        <UButton :label="t('whatsnew.gotIt')" @click="dismiss" />
      </div>
    </template>
  </UModal>
</template>
