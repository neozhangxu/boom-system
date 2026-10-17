import { defineConfig } from 'vite';

export default defineConfig({
  define: {
    'process.env.NODE_ENV': JSON.stringify('production')
  },
  server: {
    host: '0.0.0.0',
    port: 5173,
  },
  build: {
    outDir: 'static/build',
    emptyOutDir: true,
    minify: 'oxc',
    lib: {
      entry: 'frontend/vendor-entry.js',
      name: 'BOMVendor',
      formats: ['iife'],
      fileName: () => 'bom-vendor.js'
    },
    watch: process.env.VITE_WATCH ? {} : null
  }
});
