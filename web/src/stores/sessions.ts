// A shared "the session list may have changed" signal (issue #30).
//
// Two entries can create a conversation now: the Chat view and the floating
// widget. They hold separate thread instances, so neither can see the other's
// turn start -- and a conversation created in the widget would be missing from
// the sidebar until the page was reloaded, which is the "start in the corner,
// continue at full width" path the widget exists for.
//
// The creator announces it here and whoever renders the list re-reads. Same
// useSyncExternalStore shape as the toast store; too small to want a library.
import { useSyncExternalStore } from 'react'

let version = 0
const listeners = new Set<() => void>()

/**
 * Announce that a conversation may have been created since the last read. A
 * false alarm costs one list request, so callers do not have to be sure.
 */
export function invalidateSessions() {
  version++
  for (const l of listeners) l()
}

function subscribe(cb: () => void) {
  listeners.add(cb)
  return () => {
    listeners.delete(cb)
  }
}

/** The current version. A component re-reads the list when it changes. */
export function useSessionsVersion(): number {
  return useSyncExternalStore(subscribe, () => version)
}
