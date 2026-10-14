import { defineConfig } from 'vite';

export default defineConfig({
  build: {
    outDir: 'static/build',
    emptyOutDir: true,
    minify: 'esbuild',
    lib: {
      entry: 'frontend/vendor-entry.js',
      name: 'BOMVendor',
      formats: ['iife'],
      fileName: () => 'bom-vendor.js'
    },
    rollupOptions: {
      output: {
        inlineDynamicImports: true
      }
    }
  }
});
