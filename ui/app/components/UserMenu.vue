<script setup lang="ts">
import type { DropdownMenuItem } from '@nuxt/ui'

const { t, locale, locales } = useI18n()
const colorMode = useColorMode()
const { user, logout, setUserLocale } = useAuth()
const toast = useToast()
const { version } = useShell()

const initials = computed(() => {
  const u = user.value
  if (!u) return ''
  const name = [u.first_name, u.last_name].filter(Boolean)
  return (name.length ? name.map(n => n![0]).join('') : u.email.slice(0, 2)).slice(0, 2).toUpperCase()
})

const items = computed<DropdownMenuItem[][]>(() => [
  [{
    type: 'label',
    label: user.value?.email
  }],
  [{
    label: t('nav.security'),
    icon: 'i-cg-lock',
    to: '/security'
  }, {
    label: t('whatsnew.title'),
    icon: 'i-cg-release',
    to: '/whats-new'
  }, {
    label: t('user.language'),
    icon: 'i-cg-language',
    // Each language in its own name, so it can be found from any other.
    children: locales.value.map(l => ({
      label: l.name ?? l.code,
      type: 'checkbox' as const,
      checked: locale.value === l.code,
      onSelect(e: Event) {
        e.preventDefault()
        if (!isLocaleCode(l.code) || l.code === locale.value) return
        // The interface switches at once; saving it to the profile is what
        // makes it follow the user to another browser.
        setUserLocale(l.code).catch((err) => {
          toast.add({ title: t('common.failed'), description: errorMessage(err), color: 'error' })
        })
      }
    }))
  }, {
    label: t('user.appearance'),
    icon: 'i-cg-sun',
    children: (['system', 'light', 'dark'] as const).map(mode => ({
      label: t(`user.mode.${mode}`),
      icon: mode === 'light' ? 'i-cg-sun' : mode === 'dark' ? 'i-cg-moon' : 'i-cg-browser',
      type: 'checkbox' as const,
      checked: colorMode.preference === mode,
      onSelect(e: Event) {
        e.preventDefault()
        colorMode.preference = mode
      }
    }))
  }],
  ...(version.value ? [[{ type: 'label' as const, label: t('user.version', { v: version.value }) }]] : []),
  [{
    label: t('user.logout'),
    icon: 'i-cg-logout',
    onSelect() {
      void logout()
    }
  }]
])
</script>

<template>
  <UDropdownMenu
    :items="items"
    :content="{ align: 'start', side: 'top', collisionPadding: 12 }"
    :ui="{ content: 'w-(--reka-dropdown-menu-trigger-width) min-w-56' }"
  >
    <button
      type="button"
      class="flex w-full items-center gap-3 px-5 py-3 text-start transition-colors hover:bg-accented data-[state=open]:bg-accented"
      :aria-label="t('cg.shell.accountMenu', { name: displayName(user) })"
    >
      <span class="grid size-8 shrink-0 place-items-center rounded-[3px] bg-inverted text-[13px] font-semibold text-inverted" aria-hidden="true">{{ initials }}</span>
      <span class="flex min-w-0 flex-1 flex-col">
        <span class="truncate text-sm font-semibold text-highlighted">{{ displayName(user) }}</span>
        <span v-if="user" class="truncate text-[13px] text-muted">{{ t(`roles.${user.role}`) }}</span>
      </span>
      <UIcon name="i-cg-sort" class="size-4 shrink-0 text-muted" />
    </button>
  </UDropdownMenu>
</template>
