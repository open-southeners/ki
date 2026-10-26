import { mkdirSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig, type Plugin } from "vite";

const outDir = resolve(__dirname, "../internal/panel/dist");

const placeholderReadme = `# panel dist

This directory holds the built web panel. It is produced by \`cd web && bun install && bun run build\`
and is git-ignored except for this file and \`.keep\`. While \`index.html\` is absent the agent serves
a minimal built-in sign-in page instead.
`;

// emptyOutDir wipes the committed placeholders; put them back after the build.
function keepPlaceholders(): Plugin {
  return {
    name: "keep-dist-placeholders",
    apply: "build",
    closeBundle() {
      mkdirSync(outDir, { recursive: true });
      writeFileSync(resolve(outDir, ".keep"), "");
      writeFileSync(resolve(outDir, "README.md"), placeholderReadme);
    },
  };
}

export default defineConfig({
  base: "/",
  plugins: [react(), tailwindcss(), keepPlaceholders()],
  resolve: { alias: { "@": resolve(__dirname, "src") } },
  build: {
    outDir,
    emptyOutDir: true,
    sourcemap: false,
    target: "es2022",
    chunkSizeWarningLimit: 500,
    rollupOptions: {
      output: {
        // Keep the heavy vendors in their own long-cacheable chunks.
        manualChunks(id) {
          if (!id.includes("node_modules")) return undefined;
          if (/[\\/](recharts|d3-[^\\/]+|victory-vendor|decimal\.js-light|internmap)[\\/]/.test(id)) return "charts";
          if (/[\\/]@radix-ui[\\/]/.test(id)) return "radix";
          if (/[\\/](react|react-dom|react-router|react-router-dom|scheduler)[\\/]/.test(id)) return "react";
          if (/[\\/](@tanstack|qrcode)[\\/]/.test(id)) return "data";
          return undefined;
        },
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      "/rpc": { target: "http://127.0.0.1:8091", ws: true, changeOrigin: false },
      "/auth": { target: "http://127.0.0.1:8091", changeOrigin: false },
    },
  },
});
