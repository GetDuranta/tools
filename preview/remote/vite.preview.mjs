import { mergeConfig } from "/src/frontend/node_modules/vite/dist/node/index.js";
import baseConfig from "/src/frontend/website/vite.config.ts";

export default async function previewConfig(environment) {
  const hostname = process.env.PREVIEW_HOSTNAME;
  if (!hostname) throw new Error("PREVIEW_HOSTNAME is required");
  const base = typeof baseConfig === "function" ? await baseConfig(environment) : baseConfig;
  return mergeConfig(base, {
    server: {
      allowedHosts: [hostname],
      hmr: {
        clientPort: 443,
        host: hostname,
        port: 5173,
        protocol: "wss",
      },
      strictPort: true,
    },
  });
}
