import type { Metadata } from "next";
import { DocsShell } from "@/components/docs/shell";

export const metadata: Metadata = { title: "Documentation" };

export default function DocsLayout({ children }: { children: React.ReactNode }) {
  return <DocsShell>{children}</DocsShell>;
}
