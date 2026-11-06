"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, ChevronDown, ChevronRight, Pencil, Send, Trash2 } from "lucide-react";
import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import * as React from "react";
import { EmptyState, ErrorState, Facts, Page, Panel } from "@/components/console/page";
import { WebhookFormDialog } from "@/components/console/webhook-form";
import { Callout, StatusBadge, type Tone } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { CodeBlock, CopyValue } from "@/components/ui/copy";
import { Skeleton, TableSkeleton } from "@/components/ui/skeleton";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { toast, toastError } from "@/components/ui/toast";
import { ApiError } from "@/lib/api";
import { formatDateTime, formatRelative, formatShortTime } from "@/lib/format";
import { useNow } from "@/lib/hooks";
import { useProject, useProjectApi, useProjectQuery } from "@/lib/session";
import type { ApiKey, Delivery, DeliveryStatus, Items, Webhook } from "@/lib/types";

const deliveryTone: Record<DeliveryStatus, { tone: Tone; label: string }> = {
  pending: { tone: "warn", label: "Retrying" },
  delivered: { tone: "good", label: "Delivered" },
  failed: { tone: "critical", label: "Failed" },
};

function DeliveryRow({ d, canAdmin, now }: { d: Delivery; canAdmin: boolean; now: number }) {
  const [open, setOpen] = React.useState(false);
  const call = useProjectApi();
  const { projectId } = useProject();
  const qc = useQueryClient();
  const retry = useMutation<unknown, ApiError>({
    mutationFn: () => call(`/webhooks/deliveries/${d.id}/retry`, { method: "POST" }),
    onSuccess: () => {
      toast("Sending again");
      return qc.invalidateQueries({ queryKey: [projectId, "webhooks", "deliveries", d.webhook_id] });
    },
    onError: (err) => toastError(err, "Could not retry"),
  });
  const pendingFirst = d.status === "pending" && d.attempts === 0;
  const state = pendingFirst ? { tone: "neutral" as Tone, label: "Queued" } : deliveryTone[d.status];
  return (
    <>
      <Tr data-testid="delivery-row" data-status={d.status} data-event={d.event_type}>
        <Td>
          <button type="button" aria-expanded={open} className="flex items-center gap-1 text-left" onClick={() => setOpen((v) => !v)}>
            {open ? <ChevronDown className="size-3.5 text-ink-3" aria-hidden /> : <ChevronRight className="size-3.5 text-ink-3" aria-hidden />}
            <span className="mono text-xs">{d.event_type}</span>
          </button>
        </Td>
        <Td>
          <StatusBadge tone={state.tone}>{state.label}</StatusBadge>
        </Td>
        <Td className="num hidden text-right sm:table-cell">{d.attempts}</Td>
        <Td className="num text-right" data-testid="delivery-response">
          {d.response_status ?? <span className="text-ink-3">–</span>}
        </Td>
        <Td className="hidden max-w-64 truncate text-ink-2 md:table-cell" title={d.last_error} data-testid="delivery-error">
          {d.last_error || <span className="text-ink-3">–</span>}
        </Td>
        <Td className="hidden whitespace-nowrap text-ink-2 sm:table-cell" title={formatDateTime(d.created_at)}>
          {formatRelative(d.created_at, now)}
        </Td>
        <Td className="text-right">
          {canAdmin && d.status !== "pending" && (
            <Button size="sm" pending={retry.isPending} data-testid="delivery-retry" onClick={() => retry.mutate()}>
              {d.status === "failed" ? "Retry" : "Send again"}
            </Button>
          )}
        </Td>
      </Tr>
      {open && (
        <tr>
          <td colSpan={7} className="border-b bg-sunken p-3">
            <Facts
              className="p-0 pb-3"
              items={[
                { label: "Delivery ID", value: <span className="mono text-xs">{d.id}</span> },
                { label: "Attempts", value: d.attempts },
                { label: "Response status", value: d.response_status ?? "No response" },
                { label: "Queued", value: formatDateTime(d.created_at) },
                { label: "Delivered", value: d.delivered_at ? formatDateTime(d.delivered_at) : "Not delivered" },
                ...(d.status === "pending" ? [{ label: "Next attempt", value: formatShortTime(d.next_attempt_at) }] : []),
              ]}
            />
            {d.last_error && (
              <p className="mb-3 text-sm text-critical-text">
                <span className="font-medium">Last error:</span> {d.last_error}
              </p>
            )}
            <CodeBlock label="What we sent" code={JSON.stringify(d.payload, null, 2)} />
          </td>
        </tr>
      )}
    </>
  );
}

/** Rotating or revoking a key does not move the endpoints that sign with it, so this page says when one needs moving. */
function SigningKeyWarning({ hook, onEdit }: { hook: Webhook; onEdit: () => void }) {
  const { canAdmin } = useProject();
  const keys = useProjectQuery<Items<ApiKey>>(["keys"], "/keys", { enabled: canAdmin });
  const now = useNow(60_000);
  const key = keys.data?.items.find((k) => k.id === hook.api_key_id);
  if (!key) return null;
  const action = (
    <Button size="sm" onClick={onEdit}>
      Change signing key
    </Button>
  );
  if (key.revoked_at) {
    return (
      <Callout tone="critical" title="This endpoint signs with a revoked key, so every delivery fails" action={action}>
        Choose another key, then update your receiver to verify with its secret.
      </Callout>
    );
  }
  if (key.expires_at) {
    const over = Date.parse(key.expires_at) <= now;
    return (
      <Callout tone="serious" title={over ? "This endpoint signs with a key whose overlap has ended" : "This endpoint signs with a key being rotated"} action={action}>
        Move it to the replacement key, and update your receiver to verify with the new secret.
      </Callout>
    );
  }
  return null;
}

export default function WebhookPage() {
  const { id } = useParams<{ id: string }>();
  const { canAdmin, projectId } = useProject();
  const call = useProjectApi();
  const qc = useQueryClient();
  const router = useRouter();
  const now = useNow(5000);
  const [editing, setEditing] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const hook = useProjectQuery<Webhook>(["webhooks", "one", id], `/webhooks/${id}`);
  const deliveries = useProjectQuery<Items<Delivery>>(["webhooks", "deliveries", id], `/webhooks/${id}/deliveries?limit=100`, { refetchInterval: 2000, enabled: !hook.error });
  const test = useMutation<{ delivery_id: string }, ApiError>({
    mutationFn: () => call(`/webhooks/${id}/test`, { method: "POST" }),
    onSuccess: () => {
      toast("Test event queued", "It shows up below in a moment.");
      return qc.invalidateQueries({ queryKey: [projectId, "webhooks", "deliveries", id] });
    },
    onError: (err) => toastError(err, "Could not send the test"),
  });

  const back = (
    <Link href="/webhooks" className="link inline-flex items-center gap-1 text-sm text-ink-2">
      <ArrowLeft className="size-3" aria-hidden /> All endpoints
    </Link>
  );

  if (hook.error) {
    return (
      <Page title="Endpoint" description={back}>
        <Panel>
          {hook.error.status === 404 ? (
            <EmptyState title="This endpoint does not exist">
              <p>It may have been deleted, along with its delivery history.</p>
            </EmptyState>
          ) : (
            <ErrorState error={hook.error} onRetry={() => hook.refetch()} />
          )}
        </Panel>
      </Page>
    );
  }

  const h = hook.data;
  const items = deliveries.data?.items ?? [];
  const delivered = items.filter((d) => d.status === "delivered").length;
  const failed = items.filter((d) => d.status === "failed").length;
  return (
    <Page
      title={<span className="mono text-lg break-all">{h?.url ?? "Endpoint"}</span>}
      description={back}
      actions={
        h &&
        canAdmin && (
          <>
            <Button variant="primary" pending={test.isPending} onClick={() => test.mutate()}>
              <Send aria-hidden /> Send test event
            </Button>
            <Button onClick={() => setEditing(true)}>
              <Pencil aria-hidden /> Edit
            </Button>
            <Button variant="danger" onClick={() => setDeleting(true)}>
              <Trash2 aria-hidden /> Delete
            </Button>
          </>
        )
      }
    >
      {h && canAdmin && <SigningKeyWarning hook={h} onEdit={() => setEditing(true)} />}
      <Panel title="Endpoint">
        {!h ? (
          <div className="p-3">
            <Skeleton className="h-16" />
          </div>
        ) : (
          <Facts
            items={[
              { label: "Status", value: h.enabled ? <StatusBadge tone="good">On</StatusBadge> : <StatusBadge tone="neutral">Off</StatusBadge> },
              { label: "Signing key", value: <CopyValue value={h.api_key} /> },
              { label: "Events", value: h.events.length === 0 ? "All events" : h.events.join(", ") },
              { label: "Created", value: formatDateTime(h.created_at) },
              { label: "Recent deliveries", value: `${delivered} delivered, ${failed} failed`, testId: "delivery-summary" },
            ]}
          />
        )}
      </Panel>
      <Panel title="Delivery history" description="Newest first. Up to 100 shown, and kept for 30 days. A 2xx is delivered. 429 and 5xx retry up to 6 times. Any other answer fails at once.">
        {deliveries.isPending ? (
          <TableSkeleton rows={3} />
        ) : deliveries.error ? (
          <ErrorState error={deliveries.error} onRetry={() => deliveries.refetch()} />
        ) : items.length === 0 ? (
          <EmptyState title="Nothing sent yet" action={canAdmin ? <Button pending={test.isPending} onClick={() => test.mutate()}>Send test event</Button> : undefined}>
            <p>Send a test event to check your endpoint, or wait for something to happen in this project.</p>
          </EmptyState>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Event</Th>
                <Th>Status</Th>
                <Th className="hidden text-right sm:table-cell">Attempts</Th>
                <Th className="text-right">Response</Th>
                <Th className="hidden md:table-cell">Failure</Th>
                <Th className="hidden sm:table-cell">Queued</Th>
                <Th>
                  <span className="sr-only">Retry</span>
                </Th>
              </tr>
            </thead>
            <tbody>
              {items.map((d) => (
                <DeliveryRow key={d.id} d={d} canAdmin={canAdmin} now={now} />
              ))}
            </tbody>
          </Table>
        )}
      </Panel>
      <WebhookFormDialog
        hook={h}
        open={editing}
        onOpenChange={setEditing}
        onSaved={async () => {
          await qc.invalidateQueries({ queryKey: [projectId, "webhooks"] });
          toast("Endpoint saved");
        }}
      />
      <ConfirmDialog
        open={deleting}
        onOpenChange={setDeleting}
        title="Delete this endpoint?"
        description="Its delivery history goes with it. Your server stops receiving events."
        confirmLabel="Delete endpoint"
        onConfirm={async () => {
          await call(`/webhooks/${id}`, { method: "DELETE" });
          await qc.invalidateQueries({ queryKey: [projectId, "webhooks"] });
          toast("Endpoint deleted");
          router.push("/webhooks");
        }}
      />
    </Page>
  );
}
