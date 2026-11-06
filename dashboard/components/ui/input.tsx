import * as React from "react";
import { cn } from "@/lib/utils";

const field =
  "w-full rounded-sm border border-line-2 bg-surface px-2.5 text-sm text-ink placeholder:text-ink-3 focus-visible:border-ink disabled:opacity-60 aria-invalid:border-critical";

export const Input = React.forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(function Input(
  { className, ...props },
  ref,
) {
  return <input ref={ref} className={cn(field, "h-8", className)} {...props} />;
});

export const Textarea = React.forwardRef<HTMLTextAreaElement, React.TextareaHTMLAttributes<HTMLTextAreaElement>>(function Textarea(
  { className, ...props },
  ref,
) {
  return <textarea ref={ref} className={cn(field, "min-h-20 py-1.5 leading-snug", className)} {...props} />;
});
