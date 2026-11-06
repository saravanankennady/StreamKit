"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import * as React from "react";
import { EmptyState, ErrorState, Facts, Page, Panel } from "@/components/console/page";
import { StatusBadge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { TableSkeleton } from "@/components/ui/skeleton";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { toast, toastError } from "@/components/ui/toast";
import { api, ApiError } from "@/lib/api";
import { formatDateTime, formatRelative, formatShortTime, shortUserAgent } from "@/lib/format";
import { useNow } from "@/lib/hooks";
import { useProject } from "@/lib/session";
import type { Items, Session } from "@/lib/types";

function Sessions() {
  const qc = useQueryClient();
  const now = useNow(10000);
  const [ending, setEnding] = React.useState<Session | "others" | null>(null);
  const sessions = useQuery<Items<Session>, ApiError>({ queryKey: ["sessions"], queryFn: ({ signal }) => api("/auth/sessions", { signal }) });
  const others = sessions.data?.items.filter((s) => !s.current) ?? [];

  const endOne = async (s: Session) => {
    await api(`/auth/sessions/${s.id}`, { method: "DELETE" });
    if (s.current) {
      window.location.assign("/login");
      return;
    }
    await qc.invalidateQueries({ queryKey: ["sessions"] });
    toast("Session ended");
  };

  return (
    <Panel
      title="Sessions"
      description="Browsers signed in to this account. They last 14 days without activity."
      actions={
        others.length > 0 && (
          <Button size="sm" variant="danger" onClick={() => setEnding("others")}>
            End all other sessions
          </Button>
        )
      }
    >
      {sessions.isPending ? (
        <TableSkeleton rows={2} />
      ) : sessions.error ? (
        <ErrorState error={sessions.error} onRetry={() => sessions.refetch()} />
      ) : sessions.data.items.length === 0 ? (
        <EmptyState title="No sessions" />
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Client</Th>
              <Th className="hidden sm:table-cell">Address</Th>
              <Th className="hidden md:table-cell">Signed in</Th>
              <Th>Last active</Th>
              <Th className="hidden lg:table-cell">Expires</Th>
              <Th className="w-24">
                <span className="sr-only">End</span>
              </Th>
            </tr>
          </thead>
          <tbody>
            {sessions.data.items.map((s) => (
              <Tr key={s.id} data-testid="session-row" data-current={s.current}>
                <Td>
                  <span title={s.user_agent}>{shortUserAgent(s.user_agent)}</span>
                  {s.current && (
                    <StatusBadge tone="good" className="ml-2">
                      This browser
                    </StatusBadge>
                  )}
                </Td>
                <Td className="mono hidden text-xs text-ink-2 sm:table-cell">{s.ip}</Td>
                <Td className="hidden whitespace-nowrap text-ink-2 md:table-cell" title={formatDateTime(s.created_at)}>
                  {formatShortTime(s.created_at)}
                </Td>
                <Td className="whitespace-nowrap text-ink-2" title={formatDateTime(s.last_seen_at)}>
                  {formatRelative(s.last_seen_at, now)}
                </Td>
                <Td className="hidden whitespace-nowrap text-ink-2 lg:table-cell">{formatShortTime(s.expires_at)}</Td>
                <Td className="text-right">
                  <Button size="sm" variant="ghost" onClick={() => setEnding(s)}>
                    {s.current ? "Sign out" : "End"}
                  </Button>
                </Td>
              </Tr>
            ))}
          </tbody>
        </Table>
      )}
      <ConfirmDialog
        open={ending !== null}
        onOpenChange={(o) => !o && setEnding(null)}
        title={ending === "others" ? `End ${others.length} other ${others.length === 1 ? "session" : "sessions"}?` : ending?.current ? "Sign out of this browser?" : "End this session?"}
        description={ending === "others" ? "Those browsers will have to sign in again." : ending?.current ? "You will have to sign in again." : "That browser will have to sign in again."}
        confirmLabel={ending === "others" ? "End sessions" : ending?.current ? "Sign out" : "End session"}
        onConfirm={async () => {
          if (ending === "others") {
            for (const s of others) await api(`/auth/sessions/${s.id}`, { method: "DELETE" });
            await qc.invalidateQueries({ queryKey: ["sessions"] });
            toast("Other sessions ended");
          } else if (ending) {
            await endOne(ending);
          }
        }}
      />
    </Panel>
  );
}

export default function AccountPage() {
  const { me } = useProject();
  const { user, organizations } = me;
  const resetLink = useMutation<unknown, ApiError>({
    mutationFn: () => api("/auth/forgot-password", { method: "POST", body: { email: user.email } }),
    onSuccess: () => toast("Reset link sent", `Check ${user.email}.`),
    onError: (err) => toastError(err, "Could not send the link"),
  });
  return (
    <Page title="Account and sessions" description="Who you are signed in as, and where.">
      <Panel
        title="Your account"
        actions={
          <Button size="sm" pending={resetLink.isPending} onClick={() => resetLink.mutate()}>
            Email me a password reset link
          </Button>
        }
      >
        <Facts
          items={[
            { label: "Name", value: user.name },
            { label: "Email", value: user.email },
            {
              label: "Email status",
              value: user.email_verified_at ? <StatusBadge tone="good">Confirmed</StatusBadge> : <StatusBadge tone="warn">Not confirmed</StatusBadge>,
            },
            { label: "Platform administrator", value: user.is_platform_admin ? "Yes" : "No" },
            { label: "Account created", value: formatDateTime(user.created_at) },
          ]}
        />
      </Panel>
      <Panel title="Organizations">
        <Table>
          <thead>
            <tr>
              <Th>Name</Th>
              <Th>Your role</Th>
              <Th>Created</Th>
            </tr>
          </thead>
          <tbody>
            {organizations.map((o) => (
              <Tr key={o.id}>
                <Td>{o.name}</Td>
                <Td className="capitalize">{o.role}</Td>
                <Td className="text-ink-2">{formatShortTime(o.created_at)}</Td>
              </Tr>
            ))}
          </tbody>
        </Table>
      </Panel>
      <Sessions />
    </Page>
  );
}
