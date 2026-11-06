"use client";

import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import * as React from "react";
import { Toaster } from "@/components/ui/toast";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ApiError } from "@/lib/api";
import { useTheme } from "@/lib/theme";

const PUBLIC_PREFIXES = ["/login", "/register", "/forgot-password", "/reset-password", "/verify-email", "/docs"];

/** A session that ended sends you to sign in, then back to where you were. */
function onUnauthenticated(err: unknown) {
  if (!(err instanceof ApiError) || err.status !== 401) return;
  const path = window.location.pathname;
  if (PUBLIC_PREFIXES.some((p) => path.startsWith(p))) return;
  const next = encodeURIComponent(path + window.location.search);
  window.location.assign(`/login?next=${next}`);
}

function makeClient() {
  return new QueryClient({
    queryCache: new QueryCache({ onError: onUnauthenticated }),
    mutationCache: new MutationCache({ onError: onUnauthenticated }),
    defaultOptions: {
      queries: {
        staleTime: 2000,
        // A refused request will not change by asking again. Only outages are worth retrying.
        retry: (count, err) => err instanceof ApiError && (err.status === 0 || err.status >= 500) && count < 2,
        refetchOnWindowFocus: true,
      },
    },
  });
}

export function Providers({ children }: { children: React.ReactNode }) {
  const [client] = React.useState(makeClient);
  const hydrate = useTheme((s) => s.hydrate);
  React.useEffect(() => hydrate(), [hydrate]);
  return (
    <QueryClientProvider client={client}>
      <TooltipProvider delayDuration={300}>
        {children}
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>
  );
}
