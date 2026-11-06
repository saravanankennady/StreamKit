"use client";

import * as ToastPrimitive from "@radix-ui/react-toast";
import { X } from "lucide-react";
import { create } from "zustand";
import { cn } from "@/lib/utils";

interface Toast {
  id: number;
  title: string;
  description?: string;
  tone: "neutral" | "error";
}

const useToasts = create<{ toasts: Toast[]; remove: (id: number) => void }>((set) => ({
  toasts: [],
  remove: (id) => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })),
}));

let next = 1;

/** Tells someone what just happened. Errors stay longer, because they include what to do about it. */
export function toast(title: string, description?: string) {
  useToasts.setState((s) => ({ toasts: [...s.toasts.slice(-3), { id: next++, title, description, tone: "neutral" }] }));
}

export function toastError(err: unknown, title = "That did not work") {
  const description = err instanceof Error ? err.message : String(err);
  useToasts.setState((s) => ({ toasts: [...s.toasts.slice(-3), { id: next++, title, description, tone: "error" }] }));
}

export function Toaster() {
  const { toasts, remove } = useToasts();
  return (
    <ToastPrimitive.Provider swipeDirection="right">
      {toasts.map((t) => (
        <ToastPrimitive.Root
          key={t.id}
          duration={t.tone === "error" ? 9000 : 4000}
          onOpenChange={(open) => !open && remove(t.id)}
          className={cn("flex items-start gap-3 rounded-sm border bg-raised px-3 py-2.5 shadow-lg", t.tone === "error" ? "border-critical" : "border-line-2")}
        >
          <div className="min-w-0 flex-1">
            <ToastPrimitive.Title className="text-sm font-medium">{t.title}</ToastPrimitive.Title>
            {t.description && <ToastPrimitive.Description className="mt-0.5 text-sm break-words text-ink-2">{t.description}</ToastPrimitive.Description>}
          </div>
          <ToastPrimitive.Close aria-label="Dismiss" className="text-ink-3 hover:text-ink">
            <X className="size-3.5" />
          </ToastPrimitive.Close>
        </ToastPrimitive.Root>
      ))}
      <ToastPrimitive.Viewport className="fixed right-3 bottom-10 z-[80] flex w-[min(24rem,calc(100vw-1.5rem))] flex-col gap-2 outline-none" />
    </ToastPrimitive.Provider>
  );
}
