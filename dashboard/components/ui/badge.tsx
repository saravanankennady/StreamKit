import { AlertTriangle, CheckCircle2, CircleDashed, Info, OctagonAlert, type LucideIcon } from "lucide-react";
import * as React from "react";
import { cn } from "@/lib/utils";

export type Tone = "good" | "warn" | "serious" | "critical" | "neutral" | "info";

const tones: Record<Tone, { icon: LucideIcon; text: string; wash: string; mark: string }> = {
  good: { icon: CheckCircle2, text: "text-good-text", wash: "bg-good-wash", mark: "text-good-text" },
  warn: { icon: AlertTriangle, text: "text-warn-text", wash: "bg-warn-wash", mark: "text-warn-text" },
  serious: { icon: AlertTriangle, text: "text-serious-text", wash: "bg-serious-wash", mark: "text-serious-text" },
  critical: { icon: OctagonAlert, text: "text-critical-text", wash: "bg-critical-wash", mark: "text-critical-text" },
  neutral: { icon: CircleDashed, text: "text-ink-2", wash: "bg-sunken", mark: "text-ink-3" },
  info: { icon: Info, text: "text-ink-2", wash: "bg-sunken", mark: "text-ink-3" },
};

/** State is always an icon and a word, never colour alone. */
export function StatusBadge({ tone, children, icon: IconOverride, className, title }: { tone: Tone; children: React.ReactNode; icon?: LucideIcon; className?: string; title?: string }) {
  const t = tones[tone];
  const Icon = IconOverride ?? t.icon;
  return (
    <span title={title} className={cn("inline-flex items-center gap-1 rounded-xs px-1.5 py-0.5 text-xs font-medium whitespace-nowrap", t.text, t.wash, className)}>
      <Icon className={cn("size-3 shrink-0", t.mark)} aria-hidden />
      {children}
    </span>
  );
}

/** A plain label, such as an environment name. It carries no state. */
export function Tag({ className, ...props }: React.HTMLAttributes<HTMLSpanElement>) {
  return <span className={cn("inline-flex items-center rounded-xs border px-1.5 py-px text-xs whitespace-nowrap text-ink-2", className)} {...props} />;
}

export function Callout({ tone = "info", title, children, className, action }: { tone?: Tone; title?: React.ReactNode; children?: React.ReactNode; className?: string; action?: React.ReactNode }) {
  const t = tones[tone];
  const Icon = t.icon;
  return (
    <div role={tone === "critical" ? "alert" : undefined} className={cn("flex flex-wrap items-start gap-2.5 rounded-xs px-3 py-2.5 text-sm", t.wash, className)}>
      <Icon className={cn("mt-0.5 size-4 shrink-0", t.mark)} aria-hidden />
      <div className="min-w-[12rem] flex-1">
        {title && <p className="font-medium text-ink">{title}</p>}
        {children && <div className="text-ink-2">{children}</div>}
      </div>
      {action}
    </div>
  );
}
