import { cn } from "@/lib/utils";

// The mark is what a selective forwarding unit does: one stream in, many out.
export function Mark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 32 32" aria-hidden className={cn("size-6", className)}>
      <path d="M10 16 L27 6 M10 16 H27 M10 16 L27 26" stroke="currentColor" strokeWidth="2.5" strokeLinecap="square" fill="none" />
      <circle cx="8" cy="16" r="5" fill="var(--signal)" stroke="currentColor" strokeWidth="1.5" />
    </svg>
  );
}

export function Wordmark({ className }: { className?: string }) {
  return (
    <span className={cn("inline-flex items-center gap-2", className)}>
      <Mark />
      <span className="text-sm leading-none font-semibold tracking-tight">
        Elnova <span className="font-normal text-ink-2">StreamKit</span>
      </span>
    </span>
  );
}
