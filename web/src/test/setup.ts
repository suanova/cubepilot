import '@testing-library/jest-dom/vitest'

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
