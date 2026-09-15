import '@testing-library/jest-dom/vitest'
import { afterEach } from 'vitest'
import { cleanup } from '@testing-library/react'

// Testing Library only unmounts between tests when it finds a global
// `afterEach`, and this config does not turn on Vitest globals. Registering it
// here rather than in each file means a forgotten one cannot leave a rendered
// component behind for the next test to find -- which surfaces as a confusing
// "found multiple elements" rather than as anything about cleanup.
afterEach(cleanup)

// A test that reaches the network is a bug, not a flaky test: jsdom has no
// server behind it, so a forgotten fake gateway would otherwise surface as a
// confusing parse error deep inside `streamSSE` instead of at the call site
// that made the request. Failing loudly here names the real problem.
//
// `src/test/gateway.ts` installs over this; it is installed per test, not here,
// because the recording it does is per-test state.
globalThis.fetch = (() => {
  throw new Error(
    'fetch() was called with no fake gateway installed -- call installFakeGateway() in the test',
  )
}) as unknown as typeof fetch
