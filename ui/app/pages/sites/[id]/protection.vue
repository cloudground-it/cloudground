<script setup lang="ts">
/**
 * A site's password-protected paths (#26): /wp-admin or a private folder
 * behind its own username and password, asked by nginx before anything
 * else answers (POST/DELETE /api/sites/{id}/protected-paths). Each change
 * is live once saved. Below it, the maintenance page (#79).
 */
interface ProtectedPath { path: string, username: string }
const { t } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site, refresh } = useCurrentSite()

const paths = computed<ProtectedPath[]>(() => site.value.config.protected_paths ?? [])
const isWP = computed(() => site.value.type === 'wordpress' || site.value.type === 'woocommerce')
const base = computed(() => `/api/sites/${site.value.id}/protected-paths`)

// The dialog adds a path, or gives an existing one new credentials.
const editing = ref(false)
const form = reactive({ path: '', username: '', password: '', existing: false })
function open(p?: ProtectedPath) {
  Object.assign(form, { path: p?.path ?? (isWP.value && !paths.value.some(x => x.path === '/wp-admin') ? '/wp-admin' : ''), username: p?.username ?? '', password: '', existing: !!p })
  editing.value = true
}
// The server's rule, so the button says what it will refuse.
const pathOK = computed(() => /^(\/[A-Za-z0-9._~-]+)+$/.test(form.path) && !form.path.split('/').some(s => s === '.' || s === '..'))
const userOK = computed(() => /^[A-Za-z0-9._@-]{1,64}$/.test(form.username))
const passwordOK = computed(() => form.password.length >= 8 && form.password.length <= 72)
async function save() {
  if (await run(() => api.post(base.value, { path: form.path, username: form.username, password: form.password }), form.existing ? t('protect.changed') : t('protect.added', { path: form.path }))) {
    editing.value = false
    form.password = ''
    await refresh()
  }
}
async function remove(p: ProtectedPath) {
  if (await confirm({ title: t('protect.removeTitle', { path: p.path }), description: t('protect.removeHint'), danger: true, confirmLabel: t('common.remove') })) {
    if (await run(() => api.del(base.value, {}, { path: p.path }), t('protect.removed', { path: p.path }))) await refresh()
  }
}
const columns = computed(() => [
  { id: 'path', header: t('protect.path') },
  { id: 'username', header: t('protect.username') },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])
</script>

<template>
  <PanelSection :title="t('protect.title')" :description="t('protect.hint')" flush>
    <template v-if="canManage" #actions>
      <UButton icon="i-cg-plus" :label="t('protect.add')" @click="open()" />
    </template>
    <UEmpty
      v-if="!paths.length"
      icon="i-cg-lock"
      :description="t('protect.none')"
      :actions="canManage ? [{ label: t('protect.add'), icon: 'i-cg-plus', onClick: () => open() }] : []"
    />
    <UTable v-else :data="paths" :columns="columns">
      <template #path-cell="{ row: { original: p } }">
        <span class="mono text-[13px] text-highlighted">{{ p.path }}</span>
      </template>
      <template #username-cell="{ row: { original: p } }">
        <span class="mono text-[13px]">{{ p.username }}</span>
      </template>
      <template #actions-cell="{ row: { original: p } }">
        <div v-if="canManage" class="flex justify-end gap-1">
          <UButton size="sm" color="neutral" variant="ghost" icon="i-cg-key" :label="t('protect.change')" @click="open(p)" />
          <UButton size="sm" color="error" variant="ghost" square icon="i-cg-trash" :aria-label="t('protect.removeTitle', { path: p.path })" @click="remove(p)" />
        </div>
      </template>
    </UTable>
    <p v-if="isWP && paths.some(p => p.path === '/wp-admin' || p.path.startsWith('/wp-admin/'))" class="m-0 border-t border-default px-4 py-3 text-[13px] text-muted sm:px-5">
      {{ t('protect.ajaxNote') }}
    </p>
  </PanelSection>

  <SiteMaintenance class="mt-6" />

  <UModal v-model:open="editing" :title="form.existing ? t('protect.changeTitle', { path: form.path }) : t('protect.add')" :description="t('protect.dialogHint')">
    <template #body>
      <div class="flex flex-col gap-4">
        <UFormField :label="t('protect.path')" :help="t('protect.pathHelp')" :error="form.path && !pathOK ? t('protect.pathBad') : undefined">
          <UInput v-model="form.path" class="w-full font-mono" placeholder="/wp-admin" :disabled="form.existing" autofocus />
        </UFormField>
        <UFormField :label="t('protect.username')" :error="form.username && !userOK ? t('protect.userBad') : undefined">
          <UInput v-model="form.username" class="w-full font-mono" autocomplete="off" />
        </UFormField>
        <UFormField :label="t('protect.password')" :hint="t('protect.passwordRange')">
          <UInput v-model="form.password" type="password" autocomplete="new-password" class="w-full font-mono" />
        </UFormField>
      </div>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="editing = false" />
        <UButton color="primary" variant="solid" icon="i-cg-lock" :label="t('common.save')" :disabled="!pathOK || !userOK || !passwordOK" :loading="busy" @click="save" />
      </div>
    </template>
  </UModal>
</template>
