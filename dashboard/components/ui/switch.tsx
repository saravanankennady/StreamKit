"use client";

import * as SwitchPrimitive from "@radix-ui/react-switch";
import * as CheckboxPrimitive from "@radix-ui/react-checkbox";
import { Check } from "lucide-react";
import * as React from "react";
import { cn } from "@/lib/utils";

export function Switch({ className, ...props }: React.ComponentProps<typeof SwitchPrimitive.Root>) {
  return (
    <SwitchPrimitive.Root
      className={cn("relative h-4.5 w-8 shrink-0 rounded-full border border-line-2 bg-sunken data-[state=checked]:border-ink data-[state=checked]:bg-ink", className)}
      {...props}
    >
      <SwitchPrimitive.Thumb className="block size-3.5 translate-x-0.5 rounded-full bg-ink-3 transition-transform data-[state=checked]:translate-x-[15px] data-[state=checked]:bg-on-ink" />
    </SwitchPrimitive.Root>
  );
}

export function Checkbox({ className, ...props }: React.ComponentProps<typeof CheckboxPrimitive.Root>) {
  return (
    <CheckboxPrimitive.Root
      className={cn("flex size-4 shrink-0 items-center justify-center rounded-xs border border-line-2 bg-surface data-[state=checked]:border-ink data-[state=checked]:bg-ink", className)}
      {...props}
    >
      <CheckboxPrimitive.Indicator>
        <Check className="size-3 text-on-ink" strokeWidth={3} aria-hidden />
      </CheckboxPrimitive.Indicator>
    </CheckboxPrimitive.Root>
  );
}

/** A checkbox with its label and an optional explanation, all clickable. */
export function CheckRow({
  checked,
  onCheckedChange,
  label,
  hint,
  disabled,
}: {
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
  label: React.ReactNode;
  hint?: React.ReactNode;
  disabled?: boolean;
}) {
  const id = React.useId();
  return (
    <div className="flex items-start gap-2">
      <Checkbox id={id} className="mt-0.5" checked={checked} disabled={disabled} onCheckedChange={(v) => onCheckedChange(v === true)} aria-describedby={hint ? `${id}-hint` : undefined} />
      <label htmlFor={id} className="min-w-0 text-sm">
        <span className="text-ink">{label}</span>
        {hint && (
          <span id={`${id}-hint`} className="block text-xs text-ink-3">
            {hint}
          </span>
        )}
      </label>
    </div>
  );
}
