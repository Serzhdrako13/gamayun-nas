import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// Сборка кладётся в internal/web/dist и встраивается в gamayund (go:embed).
// CSP gamayund запрещает inline-скрипты и стили, поэтому всё — внешними файлами.
export default defineConfig({
  plugins: [svelte()],
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true,
    modulePreload: { polyfill: false },
    assetsInlineLimit: 0,
    cssCodeSplit: false,
  },
  server: {
    // npm run dev: API — на запущенном gamayund (самоподписанный сертификат).
    proxy: { '/api': { target: 'https://127.0.0.1:8780', secure: false } },
  },
});
