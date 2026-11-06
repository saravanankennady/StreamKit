"use client";

import * as SelectPrimitive from "@radix-ui/react-select";
import { Check, ChevronDown } from "lucide-react";
import * as React from "react";
import { cn } from "@/lib/utils";
import { useFieldContext } from "./field";

export interface SelectOption {
  value: string;
  label: React.ReactNode;
}

/** Radix Select cannot hold an empty value, so "all" filters pass one in and translate at the call site. */
export const Select = React.forwardRef<
  HTMLButtonElement,
  {
    value?: string;
    onValueChange?: (value: string) => void;
    options: readonly SelectOption[];
    placeholder?: string;
    className?: string;
    id?: string;
    disabled?: boolean;
    "aria-label"?: string;
    "aria-invalid"?: boolean;
    "aria-describedby"?: string;
  }
>(function Select({ value, onValueChange, options, placeholder, className, id, disabled, ...aria }, ref) {
  const field = useFieldContext();
  return (
    <SelectPrimitive.Root value={value} onValueChange={onValueChange} disabled={disabled}>
      <SelectPrimitive.Trigger
        ref={ref}
        id={id ?? field.id}
        aria-invalid={field["aria-invalid"]}
        aria-describedby={field["aria-describedby"]}
        {...aria}
        className={cn(
          "flex h-8 w-full items-center justify-between gap-2 rounded-sm border border-line-2 bg-surface px-2.5 text-left text-sm text-ink focus-visible:border-ink disabled:opacity-60 aria-invalid:border-critical data-[placeholder]:text-ink-3",
          className,
        )}
      >
        <span className="truncate">
          <SelectPrimitive.Value placeholder={placeholder} />
        </span>
        <SelectPrimitive.Icon>
          <ChevronDown className="size-3.5 text-ink-3" aria-hidden />
        </SelectPrimitive.Icon>
      </SelectPrimitive.Trigger>
      <SelectPrimitive.Portal>
        <SelectPrimitive.Content
          position="popper"
          sideOffset={4}
          className="z-[60] max-h-72 min-w-[var(--radix-select-trigger-width)] overflow-hidden rounded-sm border border-line-2 bg-raised shadow-lg"
        >
          <SelectPrimitive.Viewport className="p-1">
            {options.map((o) => (
              <SelectPrimitive.Item
                key={o.value}
                value={o.value}
                className="relative flex cursor-default items-center rounded-xs py-1.5 pr-7 pl-2 text-sm outline-none select-none data-[disabled]:opacity-50 data-[highlighted]:bg-sunken"
              >
                <SelectPrimitive.ItemText>{o.label}</SelectPrimitive.ItemText>
                <SelectPrimitive.ItemIndicator className="absolute right-2">
                  <Check className="size-3.5" strokeWidth={3} aria-hidden />
                </SelectPrimitive.ItemIndicator>
              </SelectPrimitive.Item>
            ))}
          </SelectPrimitive.Viewport>
        </SelectPrimitive.Content>
      </SelectPrimitive.Portal>
    </SelectPrimitive.Root>
  );
});
