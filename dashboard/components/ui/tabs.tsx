"use client";

import * as TabsPrimitive from "@radix-ui/react-tabs";
import * as React from "react";
import { cn } from "@/lib/utils";

export const Tabs = TabsPrimitive.Root;
export const TabsContent = TabsPrimitive.Content;

export function TabsList({ className, ...props }: React.ComponentProps<typeof TabsPrimitive.List>) {
  return <TabsPrimitive.List className={cn("scroll-quiet flex gap-5 overflow-x-auto border-b", className)} {...props} />;
}

export function TabsTrigger({ className, ...props }: React.ComponentProps<typeof TabsPrimitive.Trigger>) {
  return (
    <TabsPrimitive.Trigger
      className={cn(
        "-mb-px shrink-0 border-b-2 border-transparent py-2 text-sm text-ink-2 hover:text-ink data-[state=active]:border-ink data-[state=active]:font-medium data-[state=active]:text-ink",
        className,
      )}
      {...props}
    />
  );
}
