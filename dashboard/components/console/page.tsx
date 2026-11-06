"use client";

import { ArrowUpRight, Plug, ShieldAlert, WifiOff } from "lucide-react";
import Link from "next/link";
import * as React from "react";
import { Button } from "@/components/ui/button";
import { ApiError } from "@/lib/api";
import { cn } from "@/lib/utils";

/** Every console page starts with what it is, in a sentence, and what you can do from here. */
export function Page({
  title,
  description,
  actions,
  children,
  className,
}: {
  title: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("flex min-w-0 flex-col", className)}>
      <header className="flex flex-wrap items-end justify-between gap-x-6 gap-y-2 border-b px-4 pt-4 pb-3 md:px-6">
        <div className="min-w-0">
          <h1 className="text-xl font-semibold tracking-tight break-words">{title}</h1>
          {description && <p className="mt-0.5 max-w-3xl text-sm text-ink-2">{description}</p>}
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </header>
      <div className="flex flex-col gap-4 p-4 md:p-6">{children}</div>
    </div>
  );
}

/** A section of a page. Panels are ruled off rather than floated, like modules in an equipment rack. */
export function Panel({
  title,
  description,
  actions,
  children,
  className,
  bodyClassName,
  id,
}: {
  title?: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  children?: React.ReactNode;
  className?: string;
  bodyClassName?: string;
  id?: string;
}) {
  return (
    <section id={id} className={cn("min-w-0 rounded-xs border bg-surface", className)}>
      {(title || actions) && (
        <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 border-b px-3 py-2">
          <div className="min-w-0">
            {title && <h2 className="text-sm font-semibold">{title}</h2>}
            {description && <p className="text-xs text-ink-3">{description}</p>}
          </div>
          {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
        </div>
      )}
      <div className={bodyClassName}>{children}</div>
    </section>
  );
}

/** An empty list explains what would be here and how to get it there. */
export function EmptyState({
  title,
  children,
  action,
  icon: Icon,
  className,
}: {
  title: React.ReactNode;
  children?: React.ReactNode;
  action?: React.ReactNode;
  icon?: React.ComponentType<{ className?: string }>;
  className?: string;
}) {
  return (
    <div className={cn("flex flex-col items-start gap-2 px-4 py-8", className)}>
      {Icon && <Icon className="size-5 text-ink-3" aria-hidden />}
      <h3 className="text-lg font-semibold">{title}</h3>
      {children && <div className="flex w-full max-w-2xl flex-col gap-2 text-sm text-ink-2">{children}</div>}
      {action && <div className="mt-1 flex flex-wrap gap-2">{action}</div>}
    </div>
  );
}

/** A failed request says what happened and what to try. */
export function ErrorState({ error, onRetry, className }: { error: unknown; onRetry?: () => void; className?: string }) {
  const api = error instanceof ApiError ? error : null;
  const content = api?.unreachable
    ? { icon: WifiOff, title: "The server is not responding", body: api.status === 0 ? api.message : "Check that streamkit-server is running, and that this dashboard can reach it. Then try again." }
    : api?.status === 403
      ? { icon: ShieldAlert, title: "You do not have access to this", body: "Your role in this organization does not allow it. Ask an owner or admin." }
      : api?.status === 429
        ? { icon: ShieldAlert, title: "Too many requests", body: api.message }
        : { icon: Plug, title: "That did not load", body: error instanceof Error ? error.message : "Something went wrong." };
  const Icon = content.icon;
  return (
    <div role="alert" className={cn("flex flex-col items-start gap-2 px-4 py-8", className)}>
      <Icon className="size-5 text-critical-text" aria-hidden />
      <h3 className="text-lg font-semibold">{content.title}</h3>
      <p className="max-w-2xl text-sm text-ink-2">{content.body}</p>
      {onRetry && api?.status !== 403 && (
        <Button size="sm" onClick={onRetry}>
          Try again
        </Button>
      )}
    </div>
  );
}

/** For Recordings, Egress and Ingress while their backend routes do not exist yet. */
export function ServiceNotRunning({ name, children }: { name: string; children?: React.ReactNode }) {
  return (
    <EmptyState icon={Plug} title={`The ${name} service is not running`}>
      <p>
        This server answered 404 for the {name} routes, so nothing here can be listed or started. They are not part of the build of <span className="mono">streamkit-server</span> you are running. This page starts working as soon as the server provides them, with no change to the dashboard.
      </p>
      {children}
      <p>
        <Link className="link inline-flex items-center gap-0.5" href={`/docs/${name === "ingress" ? "ingress" : name === "egress" ? "egress" : "recording"}`}>
          What this service will do <ArrowUpRight className="size-3" aria-hidden />
        </Link>
      </p>
    </EmptyState>
  );
}

/** Label and value pairs, for the details of one thing. */
export function Facts({ items, className }: { items: { label: React.ReactNode; value: React.ReactNode; testId?: string; wide?: boolean }[]; className?: string }) {
  return (
    <dl className={cn("grid grid-cols-[repeat(auto-fill,minmax(9.5rem,1fr))] gap-x-6 gap-y-3 p-3", className)}>
      {items.map((item, i) => (
        <div key={i} className={cn("min-w-0", item.wide && "col-span-full sm:col-span-2")}>
          <dt className="text-xs text-ink-3">{item.label}</dt>
          <dd className="mt-0.5 min-w-0 text-sm break-words" data-testid={item.testId}>
            {item.value}
          </dd>
        </div>
      ))}
    </dl>
  );
}
