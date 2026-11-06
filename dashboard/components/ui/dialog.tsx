"use client";

import * as DialogPrimitive from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import * as React from "react";
import { cn } from "@/lib/utils";
import { Button } from "./button";

export const Dialog = DialogPrimitive.Root;
export const DialogTrigger = DialogPrimitive.Trigger;
export const DialogClose = DialogPrimitive.Close;

export function DialogContent({
  title,
  description,
  children,
  className,
  closable = true,
  ...props
}: Omit<React.ComponentProps<typeof DialogPrimitive.Content>, "title"> & {
  title: React.ReactNode;
  description?: React.ReactNode;
  closable?: boolean;
}) {
  return (
    <DialogPrimitive.Portal>
      <DialogPrimitive.Overlay className="fixed inset-0 z-40 bg-black/50" />
      <DialogPrimitive.Content
        className={cn(
          "fixed top-1/2 left-1/2 z-50 flex max-h-[min(90dvh,48rem)] w-[calc(100vw-1.5rem)] max-w-lg -translate-x-1/2 -translate-y-1/2 flex-col rounded-sm border border-line-2 bg-surface shadow-[0_24px_60px_-20px_rgba(0,0,0,0.45)]",
          className,
        )}
        {...(description ? {} : { "aria-describedby": undefined })}
        {...props}
      >
        <div className="flex items-start justify-between gap-3 border-b px-4 py-3">
          <div className="min-w-0">
            <DialogPrimitive.Title className="text-lg font-semibold">{title}</DialogPrimitive.Title>
            {description && <DialogPrimitive.Description className="mt-0.5 text-sm text-ink-2">{description}</DialogPrimitive.Description>}
          </div>
          {closable && (
            <DialogPrimitive.Close asChild>
              <Button variant="ghost" size="iconSm" aria-label="Close">
                <X />
              </Button>
            </DialogPrimitive.Close>
          )}
        </div>
        <div className="scroll-quiet min-h-0 overflow-y-auto">{children}</div>
      </DialogPrimitive.Content>
    </DialogPrimitive.Portal>
  );
}

export function DialogBody({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) {
  return <div className={cn("flex flex-col gap-3 px-4 py-4", className)} {...props} />;
}

export function DialogFooter({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) {
  return <div className={cn("flex flex-wrap items-center justify-end gap-2 border-t bg-sunken px-4 py-3", className)} {...props} />;
}

/** A panel sliding in from the side, for navigation on phones. */
export function SheetContent({ children, title, ...props }: Omit<React.ComponentProps<typeof DialogPrimitive.Content>, "title"> & { title: string }) {
  return (
    <DialogPrimitive.Portal>
      <DialogPrimitive.Overlay className="fixed inset-0 z-40 bg-black/50" />
      <DialogPrimitive.Content
        aria-describedby={undefined}
        className="fixed inset-y-0 left-0 z-50 flex w-72 max-w-[85vw] flex-col border-r border-line-2 bg-surface"
        {...props}
      >
        <DialogPrimitive.Title className="sr-only">{title}</DialogPrimitive.Title>
        {children}
      </DialogPrimitive.Content>
    </DialogPrimitive.Portal>
  );
}
