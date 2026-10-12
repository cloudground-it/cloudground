<script setup lang="ts">
/*
 * Quarry's tree: the databases the session may open; each opens on its
 * tables, views and routines (the routines are listed, never run). The
 * database and table the page shows are marked.
 */
const { t } = useI18n()
const route = useRoute()
const { dbLink, tableLink } = useStudio()
const { databases, tables, errors, loadDatabases, loadTables } = useStudioSchema()

const currentDB = computed(() => typeof route.params.db === 'string' ? route.params.db : '')
const currentTable = computed(() => typeof route.params.table === 'string' ? route.params.table : '')
const open = ref<Record<string, boolean>>({})
const filter = ref('')
const failed = ref<string | null>(null)

onMounted(async () => {
  try {
    await loadDatabases()
  } catch (e) {
    failed.value = errorMessage(e)
  }
})
watch(currentDB, (db) => {
  if (!db) return
  open.value = { ...open.value, [db]: true }
  void loadTables(db).catch(() => {})
}, { immediate: true })

function toggle(db: string) {
  const now = !open.value[db]
  open.value = { ...open.value, [db]: now }
  if (now) void loadTables(db).catch(() => {})
}
const match = (name: string) => !filter.value.trim() || name.toLowerCase().includes(filter.value.trim().toLowerCase())
const items = (db: string) => {
  const list = tables.value[db]
  return {
    tables: (list?.tables ?? []).filter(x => x.type === 'table' && match(x.name)),
    views: (list?.tables ?? []).filter(x => x.type === 'view' && match(x.name)),
    routines: (list?.routines ?? []).filter(x => match(x.name))
  }
}
</script>

<template>
  <nav class="flex flex-col gap-2 py-3" :aria-label="t('studio.databases')">
    <div class="px-3">
      <UInput v-model="filter" size="sm" icon="i-cg-search" :placeholder="t('studio.filterTree')" class="w-full" :aria-label="t('studio.filterTree')" />
    </div>
    <span class="px-4 pt-1 text-[13px] font-semibold text-muted">{{ t('studio.databases') }}</span>
    <p v-if="failed" class="m-0 px-4 text-[13px] text-error">
      {{ failed }}
    </p>
    <USkeleton v-else-if="!databases" class="mx-3 h-16" />
    <p v-else-if="!databases.length" class="m-0 px-4 text-[13px] text-muted">
      {{ t('studio.noDatabases') }}
    </p>
    <ul v-else class="m-0 flex list-none flex-col p-0">
      <li v-for="db in databases" :key="db">
        <div class="flex items-center">
          <button
            type="button"
            class="flex size-8 shrink-0 items-center justify-center text-muted hover:text-highlighted"
            :aria-expanded="!!open[db]"
            :aria-label="open[db] ? t('studio.collapse', { name: db }) : t('studio.expand', { name: db })"
            @click="toggle(db)"
          >
            <UIcon :name="open[db] ? 'i-cg-chevron-down' : 'i-cg-chevron-right'" class="size-3.5" />
          </button>
          <NuxtLink
            :to="dbLink(db)"
            class="mono flex min-w-0 flex-1 items-center gap-2 py-1.5 pe-3 text-[13px]"
            :class="db === currentDB && !currentTable ? 'font-semibold text-highlighted' : 'text-default hover:text-highlighted'"
            :aria-current="db === currentDB && !currentTable ? 'page' : undefined"
          >
            <UIcon name="i-cg-database" class="size-4 shrink-0" :class="db === currentDB ? 'text-cg-blue' : 'text-muted'" />
            <span class="truncate">{{ db }}</span>
          </NuxtLink>
        </div>
        <div v-if="open[db]" class="pb-1 ps-8">
          <p v-if="errors[db]" class="m-0 py-1 pe-3 text-xs text-error">
            {{ errors[db] }}
          </p>
          <USkeleton v-else-if="!tables[db]" class="me-3 h-10" />
          <template v-else>
            <template v-for="group in (['tables', 'views', 'routines'] as const)" :key="group">
              <template v-if="items(db)[group].length">
                <span class="block pt-1.5 pb-0.5 text-[13px] font-semibold text-muted">{{ t(`studio.group.${group}`) }}</span>
                <ul class="m-0 list-none p-0">
                  <template v-if="group !== 'routines'">
                    <li v-for="x in items(db)[group]" :key="x.name">
                      <NuxtLink
                        :to="tableLink(db, x.name)"
                        class="mono flex min-w-0 items-center gap-2 py-1 pe-3 text-[12.5px]"
                        :class="db === currentDB && x.name === currentTable ? 'bg-inverted px-1.5 font-semibold text-inverted' : 'text-default hover:text-highlighted'"
                        :aria-current="db === currentDB && x.name === currentTable ? 'page' : undefined"
                      >
                        <UIcon :name="group === 'views' ? 'i-cg-eye' : 'i-cg-table'" class="size-3.5 shrink-0 opacity-70" />
                        <span class="truncate">{{ x.name }}</span>
                      </NuxtLink>
                    </li>
                  </template>
                  <template v-else>
                    <li v-for="x in items(db).routines" :key="x.name" class="mono flex min-w-0 items-center gap-2 py-1 pe-3 text-[12.5px] text-muted" :title="t('studio.routineHint')">
                      <UIcon name="i-cg-code" class="size-3.5 shrink-0 opacity-70" />
                      <span class="truncate">{{ x.name }}</span>
                      <span class="mono ms-auto shrink-0 text-xs">{{ x.type === 'function' ? 'fn' : 'proc' }}</span>
                    </li>
                  </template>
                </ul>
              </template>
            </template>
            <p v-if="!items(db).tables.length && !items(db).views.length && !items(db).routines.length" class="m-0 py-1 pe-3 text-xs text-muted">
              {{ filter ? t('studio.noMatch') : t('dbstudio.noTables') }}
            </p>
          </template>
        </div>
      </li>
    </ul>
  </nav>
</template>
