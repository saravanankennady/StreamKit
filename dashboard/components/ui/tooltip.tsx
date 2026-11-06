"use client";

import * as TooltipPrimitive from "@radix-ui/react-tooltip";
import * as React from "react";

export const TooltipProvider = TooltipPrimitive.Provider;

export function Tooltip({ content, children, side }: { content: React.ReactNode; children: React.ReactElement; side?: "top" | "right" | "bottom" | "left" }) {
  return (
    <TooltipPrimitive.Root>
      <TooltipPrimitive.Trigger asChild>{children}</TooltipPrimitive.Trigger>
      <TooltipPrimitive.Portal>
        <TooltipPrimitive.Content side={side} sideOffset={4} className="z-[70] max-w-64 rounded-xs border border-line-2 bg-raised px-2 py-1 text-xs text-ink shadow-md">
          {content}
        </TooltipPrimitive.Content>
      </TooltipPrimitive.Portal>
    </TooltipPrimitive.Root>
  );
}
