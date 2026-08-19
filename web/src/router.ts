import { createRouter, createWebHistory } from 'vue-router'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/chat' },
    { path: '/chat', name: 'chat', component: () => import('@/views/ChatView.vue') },
    { path: '/tasks', name: 'tasks', component: () => import('@/views/TasksView.vue') },
    { path: '/audit', name: 'audit', component: () => import('@/views/AuditView.vue') },
    { path: '/agent', name: 'agent', component: () => import('@/views/AgentView.vue') },
  ],
})

export default router
