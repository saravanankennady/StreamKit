"use client";

import { BookOpen } from "lucide-react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { PRODUCT_NAME } from "@/lib/brand";
import { useLive } from "@/lib/live";
import { cn } from "@/lib/utils";
import { NAV } from "./nav";
import { Wordmark } from "./mark";

export function SideNav({ onNavigate }: { onNavigate?: () => void }) {
  const pathname = usePathname();
  const snapshot = useLive((s) => s.snapshot);
  const count = (c?: "rooms" | "participants") => (!snapshot || !c ? null : c === "rooms" ? snapshot.active_rooms : snapshot.participants);
  return (
    <nav aria-label="Main" className="scroll-quiet flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-2 py-3">
      {NAV.map(({ group, items }) => (
        <div key={group}>
          <div className="px-2 pb-1 text-xs text-ink-3">{group}</div>
          <ul>
            {items.map((item) => {
              const active = pathname === item.href || pathname.startsWith(`${item.href}/`);
              const n = count(item.count);
              return (
                <li key={item.href}>
                  <Link
                    href={item.href}
                    onClick={onNavigate}
                    aria-current={active ? "page" : undefined}
                    className={cn(
                      "group relative flex h-7.5 items-center gap-2 rounded-xs px-2 text-sm",
                      active ? "bg-sunken font-medium text-ink" : "text-ink-2 hover:bg-sunken hover:text-ink",
                    )}
                  >
                    {active && <span aria-hidden className="absolute top-1 bottom-1 -left-2 w-1 rounded-r-xs bg-signal outline outline-1 outline-ink/20" />}
                    <item.icon className={cn("size-3.5 shrink-0", active ? "text-ink" : "text-ink-3 group-hover:text-ink-2")} aria-hidden />
                    <span className="truncate">{item.label}</span>
                    {n !== null && n > 0 && <span className="num ml-auto text-xs text-ink-3">{n}</span>}
                  </Link>
                </li>
              );
            })}
          </ul>
        </div>
      ))}
      <div className="mt-auto border-t pt-2">
        <Link href="/docs" onClick={onNavigate} className="flex h-7.5 items-center gap-2 rounded-xs px-2 text-sm text-ink-2 hover:bg-sunken hover:text-ink">
          <BookOpen className="size-3.5 text-ink-3" aria-hidden />
          Documentation
        </Link>
      </div>
    </nav>
  );
}

export function Sidebar() {
  return (
    <aside className="sticky top-0 hidden h-[calc(100dvh-1.75rem)] w-52 shrink-0 flex-col border-r bg-surface lg:flex">
      <Link href="/overview" className="flex h-12 shrink-0 items-center border-b px-3" aria-label={`${PRODUCT_NAME}, overview`}>
        <Wordmark />
      </Link>
      <SideNav />
    </aside>
  );
}
