<script setup lang="ts">
/**
 * The site's own icon (GET /api/sites/{id}/favicon, internal/api SPEC §3.23)
 * in a square of fixed size, or its type's icon when it has none, while it
 * loads, and for a site that is not active. Decorative: the domain next to
 * it names the site.
 */
import type { Site } from '~/types'

const props = withDefaults(defineProps<{ site: Pick<Site, 'id' | 'type' | 'status'>, size?: 'sm' | 'lg' }>(), { size: 'sm' })

const src = ref<string | null>(null)
watchEffect(() => {
  src.value = null
  const { id, status } = props.site
  if (status !== 'active') return
  void siteFavicon(id).then(url => {
    if (props.site.id === id) src.value = url
  })
})
</script>

<template>
  <span
    class="inline-flex shrink-0 items-center justify-center rounded-(--ui-radius) border border-default bg-elevated"
    :class="size === 'lg' ? 'size-10' : 'size-8'"
    aria-hidden="true"
  >
    <img v-if="src" :src="src" alt="" class="object-contain" :class="size === 'lg' ? 'size-6' : 'size-5'">
    <UIcon v-else :name="siteTypeIcon[site.type]" class="text-muted" :class="size === 'lg' ? 'size-5' : 'size-4'" />
  </span>
</template>
