// Toast -- tiny global notification store (mirrors the original inline toast).
import { useSyncExternalStore } from 'react'

let visible = false
let message = ''
let timer: ReturnType<typeof setTimeout> | undefined
const listeners = new Set<() => void>()

function emit() {
  for (const l of listeners) l()
}

export function showToast(msg: string) {
  message = msg
  visible = true
  emit()
  clearTimeout(timer)
  timer = setTimeout(() => {
    visible = false
    emit()
  }, 2400)
}

function subscribe(cb: () => void) {
  listeners.add(cb)
  return () => {
    listeners.delete(cb)
  }
}

function getSnapshot() {
  return visible
}

function getMessage() {
  return message
}

/** React hook -- re-renders the consumer whenever the toast visibility changes. */
export function useToast() {
  const isVisible = useSyncExternalStore(subscribe, getSnapshot)
  return { visible: isVisible, message: getMessage() }
}