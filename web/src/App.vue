<script setup lang="ts">
// App shell — sidebar navigation + topbar + routed view.
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { useToastStore } from '@/stores/toast'
import { getCurrentUser } from '@/api/client'

const route = useRoute()
const toast = useToastStore()

const viewTitles: Record<string, string> = {
  chat: '对话',
  tasks: '定时任务',
  audit: '审计',
  agent: 'Agent 配置',
}
const title = computed(() => viewTitles[String(route.name)] ?? 'CubePilot')

const user = getCurrentUser()
const initials = user
  .split(/[._-]/)
  .map((p) => p[0]?.toUpperCase() ?? '')
  .slice(0, 2)
  .join('')
</script>

<template>
  <div class="app">
    <aside class="sidebar">
      <div class="brand">
        <svg class="brand-mark" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round">
          <path d="M12 2.5 21 7v10l-9 4.5L3 17V7z" />
          <path d="M3.3 7 12 11.3 20.7 7M12 11.3v9.7" />
        </svg>
        <div class="brand-text">
          <span class="brand-name">CubeStack</span>
          <span class="brand-sub">CubePilot 智能助手</span>
        </div>
      </div>
      <nav class="nav">
        <RouterLink class="nav-item" to="/chat">
          <svg class="icon" viewBox="0 0 24 24"><path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" /></svg>
          <span>对话</span>
        </RouterLink>
        <RouterLink class="nav-item" to="/tasks">
          <svg class="icon" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 3" /></svg>
          <span>定时任务</span>
        </RouterLink>
        <RouterLink class="nav-item" to="/audit">
          <svg class="icon" viewBox="0 0 24 24"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" /><path d="M9 11.5l2 2 4-4" /></svg>
          <span>审计</span>
        </RouterLink>
        <RouterLink class="nav-item" to="/agent">
          <svg class="icon" viewBox="0 0 24 24"><rect x="8" y="8" width="8" height="8" rx="1.5" /><path d="M8 5V3M12 5V3M16 5V3M8 21v-2M12 21v-2M16 21v-2M3 8h2M3 12h2M3 16h2M19 8h2M19 12h2M19 16h2" /></svg>
          <span>Agent 配置</span>
        </RouterLink>
      </nav>
      <div class="sidebar-foot">
        <div class="user">
          <div class="avatar">{{ initials }}</div>
          <div class="user-meta">
            <span class="name">{{ user }}</span>
            <span class="scope">suanova-dev / cubepilot</span>
          </div>
        </div>
      </div>
    </aside>

    <div class="main">
      <header class="topbar">
        <div class="topbar-title">
          <strong>{{ title }}</strong>
        </div>
        <div class="topbar-actions">
          <div class="search">
            <svg class="icon" style="width: 14px; height: 14px" viewBox="0 0 24 24"><circle cx="11" cy="11" r="8" /><path d="M21 21l-4.35-4.35" /></svg>
            <input placeholder="搜索资源、日志、能力…" aria-label="全局搜索" />
          </div>
          <button class="avatar-btn" aria-label="账户">{{ initials }}</button>
        </div>
      </header>

      <main class="content">
        <RouterView />
      </main>
    </div>

    <Transition name="toast">
      <div v-if="toast.visible" class="toast show" role="status">
        <svg class="icon" viewBox="0 0 24 24"><path d="M20 6L9 17l-5-5" /></svg>
        {{ toast.message }}
      </div>
    </Transition>
  </div>
</template>

<style scoped>
/* router-link active state uses the .nav-item.active styles from main.css */
.nav-item.router-link-active {
  background: var(--accent-soft);
  color: var(--accent-ink);
  font-weight: 600;
}
.nav-item.router-link-active .icon {
  color: var(--accent-strong);
}
.toast-enter-active,
.toast-leave-active {
  transition: all 0.2s;
}
.toast-enter-from,
.toast-leave-to {
  opacity: 0;
  transform: translateX(-50%) translateY(20px);
}
</style>
