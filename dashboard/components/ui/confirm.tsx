"use client";

import * as AlertDialog from "@radix-ui/react-alert-dialog";
import * as React from "react";
import { ApiError } from "@/lib/api";
import { Button } from "./button";
import { Input } from "./input";
import { Label } from "./field";

/**
 * Asks before doing something that cannot be undone. Stays open and shows the error if the action fails.
 * Set typeToConfirm to make someone type a name first, for the actions that do the most damage.
 */
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  destructive = true,
  typeToConfirm,
  onConfirm,
  children,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description: React.ReactNode;
  confirmLabel: string;
  destructive?: boolean;
  typeToConfirm?: string;
  onConfirm: () => Promise<unknown> | void;
  children?: React.ReactNode;
}) {
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [typed, setTyped] = React.useState("");
  const inputId = React.useId();

  React.useEffect(() => {
    if (open) {
      setError(null);
      setTyped("");
    }
  }, [open]);

  async function run(e: React.MouseEvent) {
    e.preventDefault();
    setPending(true);
    setError(null);
    try {
      await onConfirm();
      onOpenChange(false);
    } catch (err) {
      setError(err instanceof ApiError || err instanceof Error ? err.message : "That did not work. Try again.");
    } finally {
      setPending(false);
    }
  }

  const blocked = typeToConfirm !== undefined && typed !== typeToConfirm;

  return (
    <AlertDialog.Root open={open} onOpenChange={onOpenChange}>
      <AlertDialog.Portal>
        <AlertDialog.Overlay className="fixed inset-0 z-40 bg-black/50" />
        <AlertDialog.Content className="fixed top-1/2 left-1/2 z-50 w-[calc(100vw-1.5rem)] max-w-md -translate-x-1/2 -translate-y-1/2 rounded-sm border border-line-2 bg-surface shadow-[0_24px_60px_-20px_rgba(0,0,0,0.45)]">
          <div className="flex flex-col gap-2 px-4 py-4">
            <AlertDialog.Title className="text-lg font-semibold">{title}</AlertDialog.Title>
            <AlertDialog.Description className="text-sm text-ink-2">{description}</AlertDialog.Description>
            {children}
            {typeToConfirm !== undefined && (
              <div className="mt-2 flex flex-col gap-1">
                <Label htmlFor={inputId}>
                  Type <span className="mono">{typeToConfirm}</span> to confirm
                </Label>
                <Input id={inputId} value={typed} autoComplete="off" onChange={(e) => setTyped(e.target.value)} />
              </div>
            )}
            {error && (
              <p role="alert" className="text-sm text-critical-text">
                {error}
              </p>
            )}
          </div>
          <div className="flex flex-wrap justify-end gap-2 border-t bg-sunken px-4 py-3">
            <AlertDialog.Cancel asChild>
              <Button>Cancel</Button>
            </AlertDialog.Cancel>
            <AlertDialog.Action asChild>
              <Button variant={destructive ? "dangerSolid" : "primary"} pending={pending} disabled={blocked} onClick={run}>
                {confirmLabel}
              </Button>
            </AlertDialog.Action>
          </div>
        </AlertDialog.Content>
      </AlertDialog.Portal>
    </AlertDialog.Root>
  );
}
