<script setup lang="ts">
/*
 * The signed-out frame: the panel's sign-in, first setup, and Quarry's
 * sign-in and handoff, which show Quarry's wordmark.
 */
const { t } = useI18n()
const route = useRoute()
const colorMode = useColorMode()
const dark = computed(() => colorMode.value === 'dark')
const quarry = computed(() => isStudioRoute(route.path))
</script>

<template>
  <div class="flex min-h-svh flex-col bg-default">
    <header class="flex h-(--ui-header-height) shrink-0 items-center justify-between gap-3 border-b border-default px-5">
      <AppLogo wordmark :product="quarry ? 'quarry' : ''" />
      <UButton
        color="neutral"
        variant="ghost"
        square
        :icon="dark ? 'i-cg-sun' : 'i-cg-moon'"
        :aria-label="dark ? t('cg.shell.themeLight') : t('cg.shell.themeDark')"
        @click="colorMode.preference = dark ? 'light' : 'dark'"
      />
    </header>
    <main class="flex flex-1 items-center justify-center px-4 py-12">
      <div class="w-full max-w-md">
        <slot />
      </div>
    </main>
  </div>
</template>
