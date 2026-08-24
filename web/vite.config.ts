import { readFileSync } from "node:fs";

import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

function requireDevelopmentFile(variable: string): Buffer {
  const path = process.env[variable];
  if (!path) {
    throw new Error(
      `${variable} is required for the HTTPS development server; point it to a trusted loopback certificate file.`,
    );
  }
  return readFileSync(path);
}

export default defineConfig(({ command, isPreview }) => ({
  plugins: [react()],
  build: {
    sourcemap: false,
  },
  server:
    command === "serve" && !isPreview
      ? {
          host: "127.0.0.1",
          port: 5173,
          strictPort: true,
          https: {
            cert: requireDevelopmentFile("XTUNNEL_DEV_TLS_CERT"),
            key: requireDevelopmentFile("XTUNNEL_DEV_TLS_KEY"),
          },
          proxy: {
            // 保留浏览器可见的 Host/Origin，不为本地联调改写同源安全语义。
            "/api/v1": {
              target: "http://127.0.0.1:8080",
              changeOrigin: false,
            },
          },
        }
      : undefined,
}));
