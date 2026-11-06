import * as React from "react";
import { cn } from "@/lib/utils";

/** Scrolls inside itself on a phone, so a wide table never pushes the page sideways. */
export function Table({ className, wrapClassName, ...props }: React.TableHTMLAttributes<HTMLTableElement> & { wrapClassName?: string }) {
  return (
    <div className={cn("scroll-quiet relative w-full overflow-x-auto", wrapClassName)}>
      <table className={cn("w-full border-collapse text-sm", className)} {...props} />
    </div>
  );
}
export function Th({ className, ...props }: React.ThHTMLAttributes<HTMLTableCellElement>) {
  return <th scope="col" className={cn("border-b border-line-2 px-3 py-1.5 text-left text-xs font-medium whitespace-nowrap text-ink-3", className)} {...props} />;
}
export function Td({ className, ...props }: React.TdHTMLAttributes<HTMLTableCellElement>) {
  return <td className={cn("border-b px-3 py-2 align-middle", className)} {...props} />;
}
export function Tr({ className, ...props }: React.HTMLAttributes<HTMLTableRowElement>) {
  return <tr className={cn("hover:bg-sunken/60", className)} {...props} />;
}
