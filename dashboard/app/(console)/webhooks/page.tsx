"use client";

import { useQueryClient } from "@tanstack/react-query";
import { Plus, Webhook as WebhookIcon } from "lucide-react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import * as React from "react";
import { EmptyState, ErrorState, Page, Panel } from "@/components/console/page";
import { WebhookFormDialog } from "@/components/console/webhook-form";
import { Callout, StatusBadge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CodeBlock } from "@/components/ui/copy";
import { TableSkeleton } from "@/components/ui/skeleton";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { useProject, useProjectQuery } from "@/lib/session";
import type { Items, Webhook } from "@/lib/types";

export default function WebhooksPage() {
  const { canAdmin, projectId } = useProject();
  const hooks = useProjectQuery<Items<Webhook>>(["webhooks"], "/webhooks", { refetchInterval: 15000 });
  const [creating, setCreating] = React.useState(false);
  const router = useRouter();
  const qc = useQueryClient();
  return (
    <Page
      title="Webhooks"
      description="Tell your backend when rooms start and finish, people join and leave, and tracks are published. Failed deliveries retry for about two minutes."
      actions={
        canAdmin && (
          <Button variant="primary" onClick={() => setCreating(true)}>
            <Plus aria-hidden /> Add endpoint
          </Button>
        )
      }
    >
      {!canAdmin && <Callout title="You can look, but not change these">Your role in this organization is member. Owners and admins can manage webhooks.</Callout>}
      <Panel>
        {hooks.isPending ? (
          <TableSkeleton rows={2} />
        ) : hooks.error ? (
          <ErrorState error={hooks.error} onRetry={() => hooks.refetch()} />
        ) : hooks.data.items.length === 0 ? (
          <EmptyState icon={WebhookIcon} title="No endpoints yet">
            <p>Add a URL and we will POST a signed JSON event to it. The Authorization header carries a token signed with your API key, and its sha256 claim covers the exact body:</p>
            <CodeBlock
              label="Verifying a delivery in Node"
              code={`const claims = jwt.verify(req.headers.authorization, apiSecret, { issuer: apiKey });
const sum = crypto.createHash("sha256").update(rawBody).digest("base64");
if (claims.sha256 !== sum) throw new Error("body was changed");`}
            />
            <p>
              <Link className="link" href="/docs/webhooks">
                Read the webhook guide
              </Link>
            </p>
          </EmptyState>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Endpoint</Th>
                <Th>Status</Th>
                <Th className="hidden md:table-cell">Events</Th>
                <Th className="hidden lg:table-cell">Signed with</Th>
              </tr>
            </thead>
            <tbody>
              {hooks.data.items.map((h) => (
                <Tr key={h.id} data-testid="webhook-row" data-url={h.url}>
                  <Td className="max-w-md">
                    <Link className="link mono block truncate text-xs" href={`/webhooks/${h.id}`}>
                      {h.url}
                    </Link>
                  </Td>
                  <Td>{h.enabled ? <StatusBadge tone="good">On</StatusBadge> : <StatusBadge tone="neutral">Off</StatusBadge>}</Td>
                  <Td className="hidden text-ink-2 md:table-cell">{h.events.length === 0 ? "All events" : `${h.events.length} ${h.events.length === 1 ? "event" : "events"}`}</Td>
                  <Td className="mono hidden text-xs text-ink-2 lg:table-cell">{h.api_key}</Td>
                </Tr>
              ))}
            </tbody>
          </Table>
        )}
      </Panel>
      <WebhookFormDialog
        open={creating}
        onOpenChange={setCreating}
        onSaved={async (hook) => {
          await qc.invalidateQueries({ queryKey: [projectId, "webhooks"] });
          router.push(`/webhooks/${hook.id}`);
        }}
      />
    </Page>
  );
}
