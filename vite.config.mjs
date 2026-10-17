import { defineConfig } from 'vite';

export default defineConfig({
  define: {
    'process.env.NODE_ENV': JSON.stringify('production')
  },
  // 🛠️ 新增 server 配置：修改为 0.0.0.0:8081
  server: {
    host: '0.0.0.0',
    port: 8081,
    // 如果你希望在执行 vite 启动时，强制把打包文件写入硬盘供 Go 读取，可以取消注释下面这行
    // writeToDisk: true 
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
    // 🛠️ 核心：当运行带有 watch 的环境时，保留增量编译的高性能
    watch: process.env.VITE_WATCH ? {} : null
  }
});

