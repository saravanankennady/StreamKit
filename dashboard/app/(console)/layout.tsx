"use client";

import * as React from "react";
import { ConsoleShell } from "@/components/shell/console-shell";

export default function ConsoleLayout({ children }: { children: React.ReactNode }) {
  return (
    <ConsoleShell>
      <React.Suspense>{children}</React.Suspense>
    </ConsoleShell>
  );
}
