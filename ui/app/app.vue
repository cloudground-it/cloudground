<script setup lang="ts">
import { de, en, es, fr, it } from '@nuxt/ui/locale'

const colorMode = useColorMode()
const { locale } = useI18n()
const uiLocales = { it, en, fr, es, de }
const uiLocale = computed(() => uiLocales[locale.value as keyof typeof uiLocales] ?? it)

// Quarry's pages carry its product name in the browser tab.
const route = useRoute()
const product = computed(() => isStudioRoute(route.path) ? 'CloudGround Quarry' : 'CloudGround')
useHead({
  titleTemplate: title => (title ? `${title} · ${product.value}` : product.value),
  meta: [{ key: 'theme-color', name: 'theme-color', content: computed(() => colorMode.value === 'dark' ? '#0F0F12' : '#F2F1EC') }],
  htmlAttrs: { lang: locale }
})
</script>

<template>
  <UApp :locale="uiLocale">
    <NuxtLoadingIndicator color="var(--ui-primary)" />
    <NuxtLayout>
      <NuxtPage />
    </NuxtLayout>
  </UApp>
</template>
