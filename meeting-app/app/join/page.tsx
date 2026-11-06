"use client";

import dynamic from "next/dynamic";
import { Centered } from "@/components/ui";

// The code is in the URL's fragment, which only the browser can read, and the SDK needs a browser anyway.
const JoinFlow = dynamic(() => import("@/components/join-flow").then((m) => m.JoinFlow), {
  ssr: false,
  loading: () => (
    <Centered>
      <p className="text-sm text-ink-2">Loading&hellip;</p>
    </Centered>
  ),
});

export default function JoinPage() {
  return <JoinFlow />;
}
