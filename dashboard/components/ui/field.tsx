"use client";

import * as LabelPrimitive from "@radix-ui/react-label";
import * as React from "react";
import { cn } from "@/lib/utils";

export function Label({ className, ...props }: React.ComponentProps<typeof LabelPrimitive.Root>) {
  return <LabelPrimitive.Root className={cn("text-sm font-medium text-ink", className)} {...props} />;
}

const FieldContext = React.createContext<{ id?: string; "aria-invalid"?: boolean; "aria-describedby"?: string }>({});

/** For controls nested inside something else, such as react-hook-form's Controller, which cannot take these directly. */
export const useFieldContext = () => React.useContext(FieldContext);

/** A labelled form control with a hint and an error. The child, or a control inside it that reads the context, takes the id. */
export function Field({
  label,
  hint,
  error,
  children,
  className,
  optional,
}: {
  label: React.ReactNode;
  hint?: React.ReactNode;
  error?: string;
  children: React.ReactElement<{ id?: string; "aria-invalid"?: boolean; "aria-describedby"?: string }>;
  className?: string;
  optional?: boolean;
}) {
  const id = React.useId();
  const describedBy = error ? `${id}-error` : hint ? `${id}-hint` : undefined;
  return (
    <div className={cn("flex flex-col gap-1", className)}>
      <Label htmlFor={id}>
        {label}
        {optional && <span className="ml-1 font-normal text-ink-3">optional</span>}
      </Label>
      <FieldContext.Provider value={{ id, "aria-invalid": !!error || undefined, "aria-describedby": describedBy }}>
        {React.cloneElement(children, { id, "aria-invalid": !!error || undefined, "aria-describedby": describedBy })}
      </FieldContext.Provider>
      {error ? (
        <p id={`${id}-error`} role="alert" className="text-xs text-critical-text">
          {error}
        </p>
      ) : hint ? (
        <p id={`${id}-hint`} className="text-xs text-ink-3">
          {hint}
        </p>
      ) : null}
    </div>
  );
}
