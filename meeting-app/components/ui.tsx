import type { ButtonHTMLAttributes, ReactNode, Ref } from "react";

const VARIANTS = {
  primary: "bg-signal text-signal-ink hover:brightness-110",
  neutral: "bg-raised text-ink hover:bg-line-2",
  danger: "bg-critical text-white hover:brightness-110",
};

export function Button({ variant = "neutral", className = "", ref, ...props }: { variant?: keyof typeof VARIANTS; ref?: Ref<HTMLButtonElement> } & ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button
      type="button"
      {...props}
      ref={ref}
      className={`inline-flex min-h-10 items-center justify-center gap-2 rounded-md px-4 text-sm font-medium disabled:cursor-not-allowed disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0 ${VARIANTS[variant]} ${className}`}
    />
  );
}

/** One column in the middle of the page, for everything that is not the meeting itself. */
export function Centered({ children, wide = false }: { children: ReactNode; wide?: boolean }) {
  return (
    <main className="flex min-h-dvh items-center justify-center px-4 py-8">
      <div className={`w-full min-w-0 ${wide ? "max-w-4xl" : "max-w-md"}`}>{children}</div>
    </main>
  );
}
