<script setup lang="ts">
// The banner of an archived version: an older minor, or the release candidate
// in next/. It is rendered in the browser only, once versions.json is loaded.
import { siteRoot, useVersions } from './versions'

const { banner } = useVersions()
</script>

<template>
  <div v-if="banner" class="VersionBanner" role="note">
    <p v-if="banner.kind === 'candidate'">
      This is the documentation of release candidate {{ banner.version }}, for testing only: do not use it in
      production.
      <template v-if="banner.latest">
        The latest release is <a :href="siteRoot" target="_self">{{ banner.latest }}</a>.
      </template>
    </p>
    <!-- target="_self" makes a full page load: the site root is another VitePress app. -->
    <p v-else>
      You are reading the documentation for statica {{ banner.minor }}. The latest version is
      <a :href="siteRoot" target="_self">{{ banner.latest }}</a>.
    </p>
  </div>
</template>

<style scoped>
.VersionBanner {
  margin-bottom: 24px;
  border-radius: 8px;
  padding: 12px 16px;
  background-color: var(--vp-custom-block-warning-bg);
  color: var(--vp-custom-block-warning-text);
  font-size: 14px;
  line-height: 24px;
  overflow-wrap: break-word;
}

.VersionBanner a {
  font-weight: 500;
  color: var(--vp-c-brand-1);
  text-decoration: underline;
  text-underline-offset: 2px;
  transition: color 0.25s;
}

.VersionBanner a:hover {
  color: var(--vp-c-brand-2);
}
</style>
