"use client";

import Link from "next/link";
import * as React from "react";
import { EmptyState, Page, Panel } from "@/components/console/page";
import { ParticipantsTable, UnmuteNote } from "@/components/console/participants";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { TableSkeleton } from "@/components/ui/skeleton";
import { useNow } from "@/lib/hooks";
import { useLive } from "@/lib/live";

export default function ParticipantsPage() {
  const snapshot = useLive((s) => s.snapshot);
  const now = useNow(1000);
  const [room, setRoom] = React.useState("all");
  const [search, setSearch] = React.useState("");

  const all = snapshot?.participant_stats ?? [];
  const rooms = Array.from(new Set(all.map((p) => p.room))).sort();
  const q = search.trim().toLowerCase();
  const shown = all.filter((p) => (room === "all" || p.room === room) && (!q || p.identity.toLowerCase().includes(q) || p.name.toLowerCase().includes(q) || p.sid.toLowerCase().includes(q)));

  return (
    <Page title="Participants" description="Everyone connected to this project right now, updated every second.">
      {all.length > 0 && (
        <div className="flex flex-wrap gap-2">
          <Input className="max-w-xs" type="search" aria-label="Search participants" placeholder="Search identity, name or SID" value={search} onChange={(e) => setSearch(e.target.value)} />
          <Select
            className="w-52"
            aria-label="Room"
            value={room}
            onValueChange={setRoom}
            options={[{ value: "all", label: "All rooms" }, ...rooms.map((r) => ({ value: r, label: r }))]}
          />
        </div>
      )}
      <Panel>
        {!snapshot ? (
          <TableSkeleton rows={3} />
        ) : all.length === 0 ? (
          <EmptyState
            title="Nobody is connected"
            action={
              <Button asChild>
                <Link href="/developer?tab=tokens">Generate a token to join with</Link>
              </Button>
            }
          >
            <p>Participants appear here the moment they join a room in this project with a token signed by one of its API keys.</p>
          </EmptyState>
        ) : shown.length === 0 ? (
          <EmptyState title="No participant matches that">
            <p>Clear the search or pick another room.</p>
          </EmptyState>
        ) : (
          <ParticipantsTable participants={shown} now={now} />
        )}
      </Panel>
      {all.length > 0 && <UnmuteNote />}
    </Page>
  );
}
