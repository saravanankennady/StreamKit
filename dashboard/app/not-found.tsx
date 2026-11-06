import Link from "next/link";
import { Mark } from "@/components/shell/mark";

export default function NotFound() {
  return (
    <main className="flex min-h-dvh flex-col items-start justify-center gap-3 px-6 md:px-16">
      <Mark className="size-7" />
      <h1 className="text-2xl font-semibold tracking-tight">There is nothing at this address</h1>
      <p className="max-w-md text-ink-2">The page may have moved, or the link may be wrong.</p>
      <Link className="link" href="/overview">
        Go to the overview
      </Link>
    </main>
  );
}
