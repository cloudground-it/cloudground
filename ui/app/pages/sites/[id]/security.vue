<script setup lang="ts">
/**
 * A site's Security page: what the panel knows about threats to the site's
 * own code. The known vulnerabilities of a WordPress site's core, plugins
 * and themes (#31), and the malware scan of its files (#76).
 */
const { site } = useCurrentSite()
</script>

<template>
  <div class="flex flex-col gap-6">
    <SiteMalware v-if="site.type !== 'proxy'" />
    <SiteVulnerabilities v-if="isWordPress(site)" />
  </div>
</template>
