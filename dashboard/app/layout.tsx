import type { Metadata, Viewport } from "next";
import { PRODUCT_NAME } from "@/lib/brand";
import { THEME_SCRIPT } from "@/lib/theme";
import "./globals.css";
import { Providers } from "./providers";

export const metadata: Metadata = {
  title: { default: PRODUCT_NAME, template: `%s · ${PRODUCT_NAME}` },
  description: `Management console for ${PRODUCT_NAME}, a self-hosted real-time video platform.`,
};

export const viewport: Viewport = {
  width: "device-width",
  initialScale: 1,
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: THEME_SCRIPT }} />
      </head>
      <body>
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}
