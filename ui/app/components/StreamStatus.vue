<script setup lang="ts">
// Shown only while the live event stream is down, and only after a few
// seconds of it: a reconnect that succeeds at once is not news. Pages keep
// working meanwhile, on their old polling intervals.
const { t } = useI18n()
const { down } = useEvents()
const visible = ref(false)
let timer: ReturnType<typeof setTimeout> | undefined
watch(down, (d) => {
  clearTimeout(timer)
  if (!d) visible.value = false
  else timer = setTimeout(() => { visible.value = true }, 4000)
}, { immediate: true })
onScopeDispose(() => clearTimeout(timer))
</script>

<template>
  <Transition enter-from-class="opacity-0 translate-y-1" leave-to-class="opacity-0" enter-active-class="transition duration-200" leave-active-class="transition duration-200">
    <div
      v-if="visible"
      role="status"
      class="hud pointer-events-none fixed bottom-4 left-1/2 z-50 flex max-w-[calc(100vw-32px)] -translate-x-1/2 items-center gap-2.5 border border-(--cg-ink) bg-default px-3.5 py-2 text-default shadow-sm lg:left-auto lg:right-6 lg:translate-x-0"
    >
      <span class="size-2 shrink-0 animate-pulse bg-cg-orange" aria-hidden="true" />
      <span class="truncate">{{ t('stream.down') }}</span>
      <span class="shrink-0 text-dimmed">· {{ t('stream.retrying') }}</span>
    </div>
  </Transition>
</template>
