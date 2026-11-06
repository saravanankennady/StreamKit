"use client";

import * as DialogPrimitive from "@radix-ui/react-dialog";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, Menu } from "lucide-react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import * as React from "react";
import { Wordmark } from "@/components/shell/mark";
import { ThemeMenu } from "@/components/shell/theme-menu";
import { StatusBadge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { SheetContent } from "@/components/ui/dialog";
import { api, ApiError, httpOrigin } from "@/lib/api";
import { meKey, useProjectChoice } from "@/lib/session";
import type { Me } from "@/lib/types";
import { cn } from "@/lib/utils";
import { DOC_NAV } from "./nav";

export interface DocsAddress {
  /** Where clients connect. A real project's address when someone is signed in. */
  ws: string;
  http: string;
  real: boolean;
  projectName?: string;
}

/** Docs work signed out. Signed in, their snippets carry your project's real address. */
export function useDocsAddress(): DocsAddress {
  const me = useQuery<Me, ApiError>({ queryKey: meKey, queryFn: ({ signal }) => api<Me>("/me", { signal }), retry: false, staleTime: 60_000 });
  const choice = useProjectChoice((s) => s.projectId);
  const project = me.data?.projects.find((p) => p.id === choice) ?? me.data?.projects[0];
  if (project) return { ws: project.url, http: httpOrigin(project.url), real: true, projectName: project.name };
  return { ws: "wss://rtc.example.com", http: "https://rtc.example.com", real: false };
}

/** Asks the server itself whether a media service exists. An unknown route answers 404; a known one, 401. */
export function ServiceStatus({ path, name }: { path: string; name: string }) {
  const status = useQuery<"running" | "absent", ApiError>({
    queryKey: ["docs", "service", path],
    retry: false,
    queryFn: async ({ signal }) => {
      const res = await fetch(`/api/v1${path}`, { signal, headers: { Accept: "application/json" } });
      return res.status === 404 ? "absent" : "running";
    },
  });
  if (status.isPending) return null;
  return (
    <p>
      {status.data === "running" ? (
        <StatusBadge tone="good">{name} routes exist on this server</StatusBadge>
      ) : status.data === "absent" ? (
        <StatusBadge tone="warn">{name} is not running on this server</StatusBadge>
      ) : (
        <StatusBadge tone="neutral">Could not ask this server about {name}</StatusBadge>
      )}
    </p>
  );
}

function DocNavList({ onNavigate }: { onNavigate?: () => void }) {
  const pathname = usePathname();
  return (
    <nav aria-label="Documentation" className="scroll-quiet flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-2 py-3">
      {DOC_NAV.map(({ group, items }) => (
        <div key={group}>
          <div className="px-2 pb-1 text-xs text-ink-3">{group}</div>
          <ul>
            {items.map((item) => {
              const href = `/docs/${item.slug}`;
              const active = pathname === href;
              return (
                <li key={item.slug}>
                  <Link
                    href={href}
                    onClick={onNavigate}
                    aria-current={active ? "page" : undefined}
                    className={cn("relative flex h-7.5 items-center rounded-xs px-2 text-sm", active ? "bg-sunken font-medium text-ink" : "text-ink-2 hover:bg-sunken hover:text-ink")}
                  >
                    {active && <span aria-hidden className="absolute top-1 bottom-1 -left-2 w-1 rounded-r-xs bg-signal outline outline-1 outline-ink/20" />}
                    {item.title}
                  </Link>
                </li>
              );
            })}
          </ul>
        </div>
      ))}
    </nav>
  );
}

export function DocsShell({ children }: { children: React.ReactNode }) {
  const [open, setOpen] = React.useState(false);
  const me = useQuery<Me, ApiError>({ queryKey: meKey, queryFn: ({ signal }) => api<Me>("/me", { signal }), retry: false, staleTime: 60_000 });
  return (
    <div className="flex min-h-dvh flex-col">
      <header className="sticky top-0 z-30 flex h-12 items-center gap-2 border-b bg-surface px-3 md:px-4">
        <DialogPrimitive.Root open={open} onOpenChange={setOpen}>
          <DialogPrimitive.Trigger asChild>
            <Button variant="ghost" size="icon" className="lg:hidden" aria-label="Open documentation contents">
              <Menu aria-hidden />
            </Button>
          </DialogPrimitive.Trigger>
          <SheetContent title="Documentation contents">
            <div className="flex h-12 shrink-0 items-center border-b px-3 text-sm font-semibold">Documentation</div>
            <DocNavList onNavigate={() => setOpen(false)} />
          </SheetContent>
        </DialogPrimitive.Root>
        <Link href="/docs" aria-label="Documentation home" className="flex items-center gap-2">
          <Wordmark />
          <span className="hidden text-sm text-ink-3 sm:inline">Docs</span>
        </Link>
        <div className="ml-auto flex items-center gap-1">
          {me.data ? (
            <Button asChild size="sm" variant="secondary">
              <Link href="/overview">
                <ArrowLeft aria-hidden /> Console
              </Link>
            </Button>
          ) : (
            <Button asChild size="sm" variant="secondary">
              <Link href="/login">Sign in</Link>
            </Button>
          )}
          <ThemeMenu />
        </div>
      </header>
      <div className="flex min-h-0 flex-1">
        <aside className="sticky top-12 hidden h-[calc(100dvh-3rem)] w-56 shrink-0 flex-col border-r bg-surface lg:flex">
          <DocNavList />
        </aside>
        <main className="min-w-0 flex-1 px-4 py-6 md:px-10 md:py-8">{children}</main>
      </div>
    </div>
  );
}
