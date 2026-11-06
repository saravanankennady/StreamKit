import path from "node:path";
import type { NextConfig } from "next";

// The API is proxied so the session cookie is first-party. The live WebSocket cannot go through rewrites, so it
// connects straight to the backend: see NEXT_PUBLIC_STREAMKIT_WS_ORIGIN in .env.example. Behind nginx in production,
// /api/ never reaches this app, and the page's own origin is used for the WebSocket.
const backend = process.env.STREAMKIT_API_URL ?? "http://localhost:7880";
// Share links point at /meet on this address. In production nginx sends that to the meeting app; this does the same without it.
const meeting = process.env.STREAMKIT_MEETING_URL ?? "http://localhost:3300";

const config: NextConfig = {
  // infrastructure/docker/next-app.Dockerfile runs <app>/server.js from the standalone output.
  output: "standalone",
  outputFileTracingRoot: path.join(__dirname, ".."),
  turbopack: { root: path.join(__dirname, "..") },
  poweredByHeader: false,
  reactStrictMode: true,
  async rewrites() {
    return [
      { source: "/api/v1/:path*", destination: `${backend}/api/v1/:path*` },
      { source: "/meet", destination: `${meeting}/meet` },
      { source: "/meet/:path*", destination: `${meeting}/meet/:path*` },
    ];
  },
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "X-Frame-Options", value: "DENY" },
          { key: "X-Content-Type-Options", value: "nosniff" },
          // Reset and verification links carry a token in the URL. It must not leak through a Referer header.
          { key: "Referrer-Policy", value: "no-referrer" },
        ],
      },
    ];
  },
};

export default config;
