"use client";

import { Check, Copy } from "lucide-react";
import * as React from "react";
import { cn } from "@/lib/utils";
import { Button, type ButtonProps } from "./button";

async function writeClipboard(text: string) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(text);
    return;
  }
  // Plain HTTP on anything but localhost has no clipboard API.
  const el = document.createElement("textarea");
  el.value = text;
  el.style.position = "fixed";
  el.style.opacity = "0";
  document.body.appendChild(el);
  el.select();
  document.execCommand("copy");
  el.remove();
}

export function CopyButton({ value, label, className, size = "iconSm", variant = "ghost" }: { value: string; label?: string } & Pick<ButtonProps, "className" | "size" | "variant">) {
  const [copied, setCopied] = React.useState(false);
  return (
    <Button
      variant={variant}
      size={label ? "sm" : size}
      className={className}
      aria-label={label ? undefined : "Copy to clipboard"}
      onClick={async () => {
        await writeClipboard(value);
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      }}
    >
      {copied ? <Check aria-hidden /> : <Copy aria-hidden />}
      {label && (copied ? "Copied" : label)}
      <span role="status" className="sr-only">
        {copied ? "Copied to clipboard" : ""}
      </span>
    </Button>
  );
}

/** A value you would want to paste somewhere: an ID, a URL. */
export function CopyValue({ value, className, display }: { value: string; className?: string; display?: string }) {
  return (
    <span className={cn("inline-flex max-w-full min-w-0 items-center gap-0.5", className)}>
      <code className="mono min-w-0 truncate" title={value}>
        {display ?? value}
      </code>
      <CopyButton value={value} className="size-6 shrink-0 [&_svg]:size-3" />
    </span>
  );
}

export function CodeBlock({ code, label, className }: { code: string; label?: string; className?: string }) {
  return (
    <div className={cn("code-block relative min-w-0 rounded-xs border bg-sunken", className)}>
      {label && <div className="border-b px-3 py-1 text-xs text-ink-3">{label}</div>}
      <pre className="scroll-quiet overflow-x-auto px-3 py-2.5 pr-10 text-xs leading-relaxed">
        <code className="font-mono text-ink">{code}</code>
      </pre>
      <div className={cn("absolute right-1 rounded-xs bg-sunken", label ? "top-7" : "top-1")}>
        <CopyButton value={code} />
      </div>
    </div>
  );
}
