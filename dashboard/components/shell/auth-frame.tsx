"use client";

import Link from "next/link";
import * as React from "react";
import { api } from "@/lib/api";
import { PRODUCT_NAME } from "@/lib/brand";
import { Wordmark } from "./mark";
import { ThemeMenu } from "./theme-menu";

/** Sign-in pages are plain on purpose. The only thing to do here is the form. */
export function AuthFrame({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex min-h-dvh flex-col">
      <header className="flex h-12 items-center justify-between border-b bg-surface px-4">
        <Link href="/login" aria-label={PRODUCT_NAME}>
          <Wordmark />
        </Link>
        <div className="flex items-center gap-3">
          <Link href="/docs" className="link text-sm text-ink-2">
            Documentation
          </Link>
          <ThemeMenu />
        </div>
      </header>
      <main className="flex flex-1 items-start justify-center px-4 py-10 md:py-20">
        <div className="w-full max-w-sm">{children}</div>
      </main>
    </div>
  );
}

/** Someone already signed in has no business on the sign-in form. */
export function useRedirectIfSignedIn() {
  React.useEffect(() => {
    const controller = new AbortController();
    api("/me", { signal: controller.signal })
      .then(() => window.location.replace("/overview"))
      .catch(() => undefined);
    return () => controller.abort();
  }, []);
}

export function AuthHeading({ title, children }: { title: string; children?: React.ReactNode }) {
  return (
    <div className="mb-5">
      <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
      {children && <p className="mt-1 text-ink-2">{children}</p>}
    </div>
  );
}
