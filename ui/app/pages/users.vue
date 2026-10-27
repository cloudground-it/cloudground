<script setup lang="ts">
import type { DropdownMenuItem, TableColumn } from '@nuxt/ui'
import type { PanelUser, Role, Site } from '~/types'

const { t, locale } = useI18n()
const api = useApi()
const { user: me, isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
useHead({ title: () => t('nav.users') })

const { data: users, pending, refresh } = useLive(() => api.get<PanelUser[]>('/api/users'), { on: { types: ['user.changed'] } })
const { data: sites } = useLive(() => api.get<Site[]>('/api/sites'), { on: { types: ['site.changed', 'site.deleted'], where: ev => ev.type === 'site.deleted' || ev.data?.reason === 'created' } })
const siteName = (id: number) => sites.value?.find(s => s.id === id)?.domain ?? `#${id}`
const roles = computed(() => (['admin', 'operator', 'readonly'] as const).map(v => ({ label: t(`roles.${v}`), description: t(`roles.hint.${v}`), value: v })))
const siteItems = computed(() => (sites.value ?? []).map(s => ({ label: s.domain, value: s.id })))
const roleColor = { admin: 'primary', operator: 'secondary', readonly: 'neutral' } as const

// One modal for creating and editing.
const editing = ref<PanelUser | null>(null)
const open = ref(false)
const form = reactive({ first_name: '', last_name: '', email: '', password: '', role: 'operator' as Role, site_ids: [] as number[], disabled: false })
function openCreate() {
  editing.value = null
  Object.assign(form, { first_name: '', last_name: '', email: '', password: '', role: 'operator', site_ids: [], disabled: false })
  open.value = true
}
function openEdit(u: PanelUser) {
  editing.value = u
  Object.assign(form, { first_name: u.first_name, last_name: u.last_name, email: u.email, password: '', role: u.role, site_ids: [...(u.site_ids ?? [])], disabled: u.disabled })
  open.value = true
}
async function save() {
  const sitesFor = form.role === 'operator' ? form.site_ids : []
  const ok = editing.value
    ? await run(() => api.put(`/api/users/${editing.value!.id}`, { first_name: form.first_name, last_name: form.last_name, role: form.role, site_ids: sitesFor, disabled: form.disabled }), t('users.saved'))
    : await run(() => api.post('/api/users', { first_name: form.first_name, last_name: form.last_name, email: form.email, password: form.password, role: form.role, site_ids: sitesFor }), t('users.added'))
  if (ok) {
    open.value = false
    await refresh()
  }
}

const resetFor = ref<PanelUser | null>(null)
const newPassword = ref('')
async function resetPassword() {
  const u = resetFor.value
  if (u && await run(() => api.post(`/api/users/${u.id}/password`, { password: newPassword.value }), t('users.resetDone'))) {
    resetFor.value = null
    newPassword.value = ''
  }
}
async function remove(u: PanelUser) {
  if (await confirm({ title: t('users.removeTitle', { email: u.email }), danger: true, confirmLabel: t('common.remove') })) {
    await run(() => api.del(`/api/users/${u.id}`), t('users.removed'))
    await refresh()
  }
}
const hasTwoFactor = (u: PanelUser) => u.totp_enabled || (u.passkeys ?? 0) > 0
async function resetTwoFactor(u: PanelUser) {
  if (await confirm({ title: t('users.reset2faTitle', { email: u.email }), description: t('users.reset2faHint'), danger: true, confirmLabel: t('users.reset2fa') })) {
    await run(() => api.post(`/api/users/${u.id}/2fa/reset`, {}), t('users.reset2faDone'))
    await refresh()
  }
}
function actions(u: PanelUser): DropdownMenuItem[][] {
  return [[
    { label: t('users.edit'), icon: 'i-cg-edit', onSelect: () => openEdit(u) },
    { label: t('users.resetPassword'), icon: 'i-cg-key', disabled: u.id === me.value?.id, onSelect: () => { resetFor.value = u; newPassword.value = '' } },
    { label: t('users.reset2fa'), icon: 'i-cg-unlock', disabled: u.id === me.value?.id || !hasTwoFactor(u), onSelect: () => resetTwoFactor(u) }
  ], [
    { label: t('users.delete'), icon: 'i-cg-trash', color: 'error', disabled: u.id === me.value?.id, onSelect: () => remove(u) }
  ]]
}

const columns = computed<TableColumn<PanelUser>[]>(() => [
  { accessorKey: 'email', header: t('users.name') },
  { accessorKey: 'role', header: t('users.role'), meta: { class: { th: 'hidden sm:table-cell', td: 'hidden sm:table-cell' } } },
  { accessorKey: 'site_ids', header: t('users.sites'), meta: { class: { th: 'hidden lg:table-cell', td: 'hidden lg:table-cell' } } },
  { accessorKey: 'totp_enabled', header: '2FA', meta: { class: { th: 'hidden sm:table-cell', td: 'hidden sm:table-cell' } } },
  { accessorKey: 'disabled', header: t('users.status'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { accessorKey: 'created_at', header: t('users.created'), meta: { class: { th: 'hidden xl:table-cell', td: 'hidden xl:table-cell' } } },
  { id: 'actions', header: '', meta: { class: { td: 'text-right' } } }
])
</script>

<template>
  <UDashboardPanel id="users">
    <template #header>
      <PageNavbar :title="t('nav.users')" />
    </template>
    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('nav.users')" :description="t('ux.pages.users')">
          <template v-if="isAdmin" #links>
            <UButton icon="i-cg-user-plus" :label="t('users.add')" @click="openCreate" />
          </template>
        </UPageHeader>
        <PanelSection flush :label="t('nav.users')">
          <UTable
            :data="users ?? []"
            :columns="columns"
            :loading="pending && !users"
          >
            <template #email-cell="{ row }">
              <div class="flex items-center gap-3">
                <UAvatar :alt="displayName(row.original)" size="sm" />
                <div class="min-w-0 max-w-[11rem] sm:max-w-xs">
                  <p class="flex items-center gap-2 font-medium text-highlighted">
                    <span class="truncate">{{ displayName(row.original) }}</span>
                    <UBadge v-if="row.original.id === me?.id" size="sm" color="primary" variant="subtle" :label="t('users.you')" />
                  </p>
                  <p v-if="row.original.first_name || row.original.last_name" class="truncate text-xs text-muted">
                    {{ row.original.email }}
                  </p>
                  <p class="text-xs text-muted sm:hidden">
                    {{ t(`roles.${row.original.role}`) }}<template v-if="row.original.disabled"> · {{ t('users.disabledLabel') }}</template>
                  </p>
                </div>
              </div>
            </template>
            <template #role-cell="{ row }">
              <UBadge :color="roleColor[row.original.role]" variant="subtle" :label="t(`roles.${row.original.role}`)" />
            </template>
            <template #site_ids-cell="{ row }">
              <span v-if="row.original.role !== 'operator'" class="text-sm text-muted">{{ t('users.allSites') }}</span>
              <UTooltip v-else-if="(row.original.site_ids ?? []).length" :text="(row.original.site_ids ?? []).map(siteName).join(', ')">
                <span class="flex max-w-xs items-center gap-1.5 text-sm">
                  <span class="truncate">{{ siteName(row.original.site_ids![0]!) }}</span>
                  <UBadge v-if="row.original.site_ids!.length > 1" size="sm" color="neutral" variant="subtle" :label="`+${row.original.site_ids!.length - 1}`" />
                </span>
              </UTooltip>
              <span v-else class="text-sm text-muted">—</span>
            </template>
            <template #totp_enabled-cell="{ row }">
              <UIcon :name="hasTwoFactor(row.original) ? 'i-cg-shield-check' : 'i-cg-unlock'" class="size-5" :class="hasTwoFactor(row.original) ? 'text-success' : 'text-dimmed'" />
            </template>
            <template #disabled-cell="{ row }">
              <UBadge :color="row.original.disabled ? 'neutral' : 'success'" variant="subtle" :label="row.original.disabled ? t('users.disabledLabel') : t('users.active')" />
            </template>
            <template #created_at-cell="{ row }">
              <span class="text-sm text-muted">{{ fmtDateTime(row.original.created_at, locale) }}</span>
            </template>
            <template #actions-cell="{ row }">
              <UDropdownMenu v-if="isAdmin" :items="actions(row.original)" :content="{ align: 'end' }">
                <UButton icon="i-cg-more-vertical" color="neutral" variant="ghost" square :aria-label="t('users.actions')" />
              </UDropdownMenu>
            </template>
          </UTable>
        </PanelSection>

        <UModal v-model:open="open" :title="editing ? t('users.edit') : t('users.add')" :description="editing?.email">
          <template #body>
            <UForm id="user-form" :state="form" class="space-y-4" @submit="save">
              <div class="grid gap-4 sm:grid-cols-2">
                <UFormField :label="t('users.firstName')">
                  <UInput v-model="form.first_name" class="w-full" autocomplete="given-name" />
                </UFormField>
                <UFormField :label="t('users.lastName')">
                  <UInput v-model="form.last_name" class="w-full" autocomplete="family-name" />
                </UFormField>
              </div>
              <template v-if="!editing">
                <UFormField :label="t('common.email')" required>
                  <UInput v-model="form.email" type="email" class="w-full" autocomplete="off" />
                </UFormField>
                <UFormField :label="t('common.password')" :hint="t('auth.min12')" required>
                  <UInput v-model="form.password" type="password" class="w-full" autocomplete="new-password" />
                </UFormField>
              </template>
              <UFormField :label="t('users.role')">
                <URadioGroup v-model="form.role" :items="roles" :disabled="editing?.id === me?.id" />
              </UFormField>
              <UFormField v-if="form.role === 'operator'" :label="t('users.sites')" required>
                <USelectMenu v-model="form.site_ids" :items="siteItems" value-key="value" multiple class="w-full" :placeholder="t('users.pickSites')" />
              </UFormField>
              <USwitch v-if="editing" v-model="form.disabled" :label="t('users.disable')" :description="t('users.disableHint')" :disabled="editing.id === me?.id" />
            </UForm>
          </template>
          <template #footer>
            <div class="flex w-full justify-end gap-2">
              <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="open = false" />
              <UButton color="primary" variant="solid" type="submit" form="user-form" :label="editing ? t('common.save') : t('users.add')" :loading="busy" :disabled="form.role === 'operator' && !form.site_ids.length" />
            </div>
          </template>
        </UModal>

        <UModal :open="!!resetFor" :title="resetFor ? t('users.resetTitle', { email: resetFor.email }) : ''" @update:open="v => { if (!v) resetFor = null }">
          <template #body>
            <UFormField :label="t('security.new')" :hint="t('auth.min12')">
              <UInput v-model="newPassword" type="password" autocomplete="new-password" class="w-full" autofocus />
            </UFormField>
          </template>
          <template #footer>
            <div class="flex w-full justify-end gap-2">
              <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="resetFor = null" />
              <UButton color="primary" variant="solid" :label="t('users.resetPassword')" :disabled="newPassword.length < 12" :loading="busy" @click="resetPassword" />
            </div>
          </template>
        </UModal>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
