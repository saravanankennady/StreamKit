import { cn } from "@/lib/utils";

export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden className={cn("skeleton h-4 rounded-xs", className)} />;
}

export function TableSkeleton({ rows = 5 }: { rows?: number }) {
  return (
    <div role="status" aria-label="Loading" className="flex flex-col">
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="flex items-center gap-4 border-b px-3 py-3">
          <Skeleton className="h-3.5 w-1/4" />
          <Skeleton className="h-3.5 w-1/6" />
          <Skeleton className="h-3.5 w-1/5" />
          <Skeleton className="ml-auto h-3.5 w-16" />
        </div>
      ))}
    </div>
  );
}
