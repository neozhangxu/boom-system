import { defineConfig } from 'vite';

export default defineConfig({
  define: {
    'process.env.NODE_ENV': JSON.stringify('production')
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
    }
  }
});
