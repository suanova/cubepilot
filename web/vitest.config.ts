import { defineConfig, mergeConfig } from 'vitest/config'
// The extension is required: Vite's next-generation config loader will not
// resolve an extensionless specifier, and warns about it today.
import viteConfig from './vite.config.ts'

// The test run reuses the app's Vite config so `@/` resolves to `src/` exactly
// as it does in a build -- a test that imports through `@/` is then exercising
// the same module graph the app ships, not a second one that merely looks like
// it.
export default mergeConfig(
  viteConfig,
  defineConfig({
    test: {
      environment: 'jsdom',
      setupFiles: ['./src/test/setup.ts'],
      include: ['src/**/*.test.{ts,tsx}'],
    },
  }),
)
