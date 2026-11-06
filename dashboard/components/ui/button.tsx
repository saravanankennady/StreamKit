"use client";

import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import { Loader2 } from "lucide-react";
import * as React from "react";
import { cn } from "@/lib/utils";

export const buttonStyles = cva(
  "inline-flex shrink-0 items-center justify-center gap-1.5 whitespace-nowrap rounded-sm font-medium transition-colors select-none disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-3.5 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        primary: "bg-ink text-on-ink hover:opacity-85",
        secondary: "border border-line-2 bg-surface text-ink hover:bg-sunken",
        ghost: "text-ink-2 hover:bg-sunken hover:text-ink",
        danger: "border border-critical/50 bg-transparent text-critical-text hover:bg-critical-wash",
        dangerSolid: "bg-critical text-white hover:opacity-90",
        link: "text-ink underline decoration-line-2 underline-offset-3 hover:decoration-current",
      },
      size: {
        sm: "h-7 px-2 text-xs",
        md: "h-8 px-3 text-sm",
        lg: "h-10 px-4 text-base",
        icon: "size-8",
        iconSm: "size-7",
      },
    },
    defaultVariants: { variant: "secondary", size: "md" },
  },
);

export interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement>, VariantProps<typeof buttonStyles> {
  asChild?: boolean;
  pending?: boolean;
}

export const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { className, variant, size, asChild, pending, children, disabled, type, ...props },
  ref,
) {
  if (asChild) {
    return (
      <Slot ref={ref} className={cn(buttonStyles({ variant, size }), className)} {...props}>
        {children}
      </Slot>
    );
  }
  return (
    <button
      ref={ref}
      type={type ?? "button"}
      disabled={disabled || pending}
      aria-busy={pending || undefined}
      className={cn(buttonStyles({ variant, size }), className)}
      {...props}
    >
      {pending && <Loader2 className="animate-spin" aria-hidden />}
      {children}
    </button>
  );
});
