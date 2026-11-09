<script setup lang="ts">
// The version selector: in the navigation bar on wide screens, and in the menu
// screen on narrow ones (screen). It shows this build's version; a transparent
// native select over it opens the list. With a single version it shows its
// name only.
import { useVersions } from './versions'

defineProps<{ screen?: boolean }>()

const { options, open } = useVersions()

function change(event: Event) {
  const select = event.target as HTMLSelectElement
  const href = select.value
  // Keep showing this version, also if the browser restores this page later.
  select.value = options.value.current.href
  if (href !== options.value.current.href) open(href)
}
</script>

<template>
  <div class="VersionSelector" :class="{ screen }">
    <span v-if="screen" class="text" aria-hidden="true">Version</span>
    <span class="picker" :class="{ menu: options.list.length > 1 }" title="Documentation version">
      <span class="name">{{ options.current.name }}</span>
      <template v-if="options.list.length > 1">
        <span class="vpi-chevron-down chevron" aria-hidden="true" />
        <select aria-label="Documentation version" :value="options.current.href" @change="change">
          <option v-for="option in options.list" :key="option.href" :value="option.href">{{ option.label }}</option>
        </select>
      </template>
    </span>
  </div>
</template>

<style scoped>
.VersionSelector {
  display: none;
  align-items: center;
}

@media (min-width: 768px) {
  .VersionSelector:not(.screen) {
    display: flex;
  }

  .VersionSelector:not(.screen)::before {
    margin-right: 12px;
    margin-left: 16px;
    width: 1px;
    height: 24px;
    background-color: var(--vp-c-divider);
    content: '';
  }
}

.VersionSelector.screen {
  display: flex;
  justify-content: space-between;
  gap: 16px;
  margin-top: 16px;
  border-radius: 8px;
  padding: 12px 14px 12px 16px;
  background-color: var(--vp-c-bg-soft);
}

.text {
  line-height: 24px;
  font-size: 12px;
  font-weight: 500;
  color: var(--vp-c-text-2);
}

.picker {
  position: relative;
  display: flex;
  align-items: center;
  gap: 4px;
  border-radius: 4px;
  line-height: 24px;
  font-size: 14px;
  font-weight: 500;
  color: var(--vp-c-text-1);
  transition: color 0.25s;
}

.picker.menu:hover {
  color: var(--vp-c-brand-1);
}

.picker:has(select:focus-visible) {
  outline: 2px solid var(--vp-c-brand-1);
  outline-offset: 2px;
}

.chevron {
  width: 14px;
  height: 14px;
}

/* Covers the name and the chevron, so the native list opens on a click or a
   tap. 16px keeps mobile browsers from zooming in on focus. */
select {
  position: absolute;
  inset: 0;
  width: 100%;
  opacity: 0;
  font-size: 16px;
  cursor: pointer;
}
</style>
