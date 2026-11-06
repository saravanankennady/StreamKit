import * as React from "react";
import { AuthFrame } from "@/components/shell/auth-frame";

export default function AuthLayout({ children }: { children: React.ReactNode }) {
  return (
    <AuthFrame>
      <React.Suspense>{children}</React.Suspense>
    </AuthFrame>
  );
}
