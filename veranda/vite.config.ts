import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  plugins: [sveltekit()],
  clearScreen: false,
  resolve: {
    conditions: ['browser']
  },
  server: {
    port: 1420,
    strictPort: true,
    watch: {
      ignored: ['**/src-tauri/**']
    }
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./vitest.setup.ts']
  }
});
