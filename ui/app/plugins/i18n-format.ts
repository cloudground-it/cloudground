// Keeps the formatters in utils/format.ts on the active interface language,
// so dates, numbers and sizes follow it even where a caller passes no locale.
export default defineNuxtPlugin({
  name: 'i18n-format',
  dependsOn: ['i18n:plugin'],
  setup(nuxtApp) {
    const locale = nuxtApp.$i18n.locale
    watch(locale, (code) => {
      if (isLocaleCode(code)) formatLocale.value = code
    }, { immediate: true })
  }
})
