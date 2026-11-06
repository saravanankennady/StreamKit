"use client";

import * as Menu from "@radix-ui/react-dropdown-menu";
import * as React from "react";
import { cn } from "@/lib/utils";

export const Dropdown = Menu.Root;
export const DropdownTrigger = Menu.Trigger;
export const DropdownSeparator = (props: React.ComponentProps<typeof Menu.Separator>) => <Menu.Separator className="my-1 h-px bg-line" {...props} />;

export function DropdownContent({ className, align = "end", ...props }: React.ComponentProps<typeof Menu.Content>) {
  return (
    <Menu.Portal>
      <Menu.Content
        align={align}
        sideOffset={4}
        className={cn("z-[60] max-h-[70dvh] min-w-44 overflow-y-auto rounded-sm border border-line-2 bg-raised p-1 shadow-lg", className)}
        {...props}
      />
    </Menu.Portal>
  );
}

export function DropdownItem({ className, ...props }: React.ComponentProps<typeof Menu.Item>) {
  return (
    <Menu.Item
      className={cn(
        "flex cursor-default items-center gap-2 rounded-xs px-2 py-1.5 text-sm outline-none select-none data-[disabled]:opacity-50 data-[highlighted]:bg-sunken [&_svg]:size-3.5 [&_svg]:text-ink-3",
        className,
      )}
      {...props}
    />
  );
}

export function DropdownLabel({ className, ...props }: React.ComponentProps<typeof Menu.Label>) {
  return <Menu.Label className={cn("px-2 py-1 text-xs text-ink-3", className)} {...props} />;
}
