// Toast — tiny global notification store (mirrors the original inline toast).
import { defineStore } from 'pinia'

let timer: ReturnType<typeof setTimeout> | undefined

export const useToastStore = defineStore('toast', {
  state: () => ({ visible: false, message: '' }),
  actions: {
    show(msg: string) {
      this.message = msg
      this.visible = true
      clearTimeout(timer)
      timer = setTimeout(() => {
        this.visible = false
      }, 2400)
    },
  },
})
