<script setup lang="ts">
/**
 * "On this page": the sections of a long page as anchor links. From lg up
 * a sticky list beside the content, the section in view highlighted while
 * the page scrolls; below lg a horizontal row of links above it (scrolls
 * sideways when it does not fit). Each section needs a stable id.
 */
const props = defineProps<{ items: { id: string, label: string }[] }>()
const { t } = useI18n()
const active = ref(props.items[0]?.id ?? '')
const root = ref<HTMLElement | null>(null)

// The panel's body is the page's scroller (UDashboardPanel), not the window.
let scroller: HTMLElement | null = null
function update() {
  if (!scroller) return
  const top = scroller.getBoundingClientRect().top + 96
  let current = props.items[0]?.id ?? ''
  for (const it of props.items) {
    const el = document.getElementById(it.id)
    if (el && el.getBoundingClientRect().top <= top) current = it.id
  }
  // At the bottom the last section is the one being read.
  if (scroller.scrollTop + scroller.clientHeight >= scroller.scrollHeight - 4) current = props.items[props.items.length - 1]?.id ?? current
  active.value = current
}
function go(id: string, e: Event) {
  e.preventDefault()
  const el = document.getElementById(id)
  if (!el) return
  el.scrollIntoView({ behavior: 'smooth', block: 'start' })
  history.replaceState(history.state, '', `#${id}`)
  active.value = id
  // Move the keyboard focus with the reader.
  el.setAttribute('tabindex', '-1')
  el.focus({ preventScroll: true })
}
onMounted(() => {
  scroller = root.value?.closest<HTMLElement>('[data-slot=body]') ?? null
  scroller?.addEventListener('scroll', update, { passive: true })
  const hash = location.hash.slice(1)
  if (hash && props.items.some(i => i.id === hash)) nextTick(() => document.getElementById(hash)?.scrollIntoView({ block: 'start' }))
  nextTick(update)
})
onBeforeUnmount(() => scroller?.removeEventListener('scroll', update))
</script>

<template>
  <nav ref="root" :aria-label="t('ux.toc.label')" class="min-w-0 lg:sticky lg:top-6 lg:self-start">
    <p class="hud m-0 mb-2 hidden text-muted lg:block">
      {{ t('ux.toc.label') }}
    </p>
    <ul class="scrollbar-none m-0 flex list-none gap-1 overflow-x-auto overflow-y-hidden border-b border-default p-0 lg:flex-col lg:gap-0 lg:overflow-visible lg:border-b-0 lg:border-s">
      <li v-for="it in items" :key="it.id" class="shrink-0">
        <a
          :href="`#${it.id}`"
          :aria-current="active === it.id ? 'location' : undefined"
          class="block px-3 py-2 text-sm whitespace-nowrap transition-colors lg:-ms-px lg:border-s-2 lg:whitespace-normal"
          :class="active === it.id ? 'font-medium text-highlighted border-cg-blue max-lg:shadow-[inset_0_-2px_0_var(--cg-blue)]' : 'text-muted hover:text-highlighted border-transparent'"
          @click="go(it.id, $event)"
        >{{ it.label }}</a>
      </li>
    </ul>
  </nav>
</template>
