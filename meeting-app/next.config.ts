import path from "node:path";
import type { NextConfig } from "next";

// nginx sends /meet here and /api/ to the server. The rewrite lets the app also run on its own, or behind the
// dashboard's own rewrite of /meet in development.
const backend = process.env.STREAMKIT_API_URL ?? "http://localhost:7880";

const config: NextConfig = {
  basePath: "/meet",
  // infrastructure/docker/next-app.Dockerfile runs <app>/server.js from the standalone output.
  output: "standalone",
  outputFileTracingRoot: path.join(__dirname, ".."),
  turbopack: {
    root: path.join(__dirname, ".."),
    // The SDKs are linked in from ../sdk-*, each with its own node_modules for its tests. Two copies of React break hooks.
    resolveAlias: { react: "./meeting-app/node_modules/react", "react-dom": "./meeting-app/node_modules/react-dom" },
  },
  poweredByHeader: false,
  reactStrictMode: true,
  async rewrites() {
    return [{ source: "/api/v1/:path*", destination: `${backend}/api/v1/:path*`, basePath: false }];
  },
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "X-Frame-Options", value: "DENY" },
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "Referrer-Policy", value: "no-referrer" },
          { key: "Permissions-Policy", value: "camera=(self), microphone=(self), display-capture=(self), speaker-selection=(self)" },
        ],
      },
    ];
  },
};

export default config;
