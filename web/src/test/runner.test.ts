import { describe, expect, it } from 'vitest'

// Proves the runner, the DOM environment and the DOM matchers are all wired up.
// Deliberately trivial: when this fails, the cause is configuration, and every
// other test's failure is noise on top of it.
describe('test runner', () => {
  it('runs with a DOM and the jest-dom matchers', () => {
    const el = document.createElement('div')
    el.textContent = 'ready'
    document.body.appendChild(el)

    expect(el).toBeInTheDocument()
    expect(el).toHaveTextContent('ready')
  })
})
