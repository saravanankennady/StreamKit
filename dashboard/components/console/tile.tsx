import * as React from "react";
import { cn } from "@/lib/utils";

/** A grid of readouts sharing hairlines, like an instrument panel. */
export function TileGrid({ className, children }: { className?: string; children: React.ReactNode }) {
  return <div className={cn("grid gap-px overflow-hidden rounded-xs border bg-line", className)}>{children}</div>;
}

export function Tile({
  label,
  value,
  detail,
  children,
  testId,
  className,
}: {
  label: React.ReactNode;
  value: React.ReactNode;
  detail?: React.ReactNode;
  children?: React.ReactNode;
  testId?: string;
  className?: string;
}) {
  return (
    <div className={cn("flex min-w-0 flex-col gap-1 bg-surface px-3 py-2.5", className)}>
      <div className="truncate text-xs text-ink-3">{label}</div>
      <div className="flex min-h-7 items-baseline gap-2">
        <div className="text-xl leading-7 font-semibold tracking-tight" data-testid={testId}>
          {value}
        </div>
      </div>
      {detail && <div className="text-xs leading-snug text-ink-3">{detail}</div>}
      {children}
    </div>
  );
}

/** Text for a figure the server has not measured. It is never shown as zero. */
export function NotMeasured({ children = "Not measured" }: { children?: React.ReactNode }) {
  return <span className="text-sm font-normal text-ink-3">{children}</span>;
}

/** A ratio against a limit. The fill only takes a status colour once the value means something. */
export function Meter({ percent, label }: { percent: number; label: string }) {
  const clamped = Math.max(0, Math.min(100, percent));
  const tone = clamped >= 90 ? "bg-critical" : clamped >= 75 ? "bg-warn" : "bg-ink-3";
  return (
    <div role="meter" aria-label={label} aria-valuenow={Math.round(clamped)} aria-valuemin={0} aria-valuemax={100} className="h-1 w-full overflow-hidden rounded-full bg-line">
      <div className={cn("h-full rounded-full", tone)} style={{ width: `${clamped}%` }} />
    </div>
  );
}

/** A tiny line without axes, for the last few minutes of a tile's value. */
export function Sparkline({ values, className }: { values: (number | null)[]; className?: string }) {
  const w = 120;
  const h = 22;
  const nums = values.filter((v): v is number => v !== null);
  if (nums.length < 2) return <div className={cn("h-[22px]", className)} aria-hidden />;
  const max = Math.max(...nums);
  const min = Math.min(...nums);
  const span = max - min || 1;
  const step = w / Math.max(values.length - 1, 1);
  let d = "";
  let pen = false;
  values.forEach((v, i) => {
    if (v === null) {
      pen = false;
      return;
    }
    const x = i * step;
    const y = h - 2 - ((v - min) / span) * (h - 4);
    d += `${pen ? "L" : "M"}${x.toFixed(1)} ${y.toFixed(1)}`;
    pen = true;
  });
  return (
    <svg aria-hidden viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className={cn("h-[22px] w-full", className)}>
      <path d={d} fill="none" stroke="var(--ink-3)" strokeWidth={1.5} vectorEffect="non-scaling-stroke" strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  );
}
