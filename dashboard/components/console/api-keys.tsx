"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { KeyRound, Plus } from "lucide-react";
import * as React from "react";
import { Controller, useForm } from "react-hook-form";
import { Callout, StatusBadge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { CopyButton, CopyValue } from "@/components/ui/copy";
import { Dialog, DialogBody, DialogContent, DialogFooter } from "@/components/ui/dialog";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { TableSkeleton } from "@/components/ui/skeleton";
import { CheckRow } from "@/components/ui/switch";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { toast } from "@/components/ui/toast";
import { ApiError, isForbidden } from "@/lib/api";
import { formatDateTime, formatRelative, formatShortTime } from "@/lib/format";
import { useNow } from "@/lib/hooks";
import { useProject, useProjectApi, useProjectQuery } from "@/lib/session";
import type { ApiKey, Items } from "@/lib/types";
import { EmptyState, ErrorState, Panel } from "./page";

/**
 * The one moment a secret exists in the browser. It lives in this component's state and nowhere else, and the only
 * way out is to say it has been saved. Nothing else can show it again, because the API will not return it.
 */
export function SecretReveal({ apiKey, onDone, heading }: { apiKey: ApiKey | null; onDone: () => void; heading: string }) {
  const [saved, setSaved] = React.useState(false);
  React.useEffect(() => {
    if (apiKey) setSaved(false);
  }, [apiKey]);
  return (
    <Dialog open={!!apiKey} onOpenChange={() => undefined}>
      <DialogContent title={heading} closable={false} onEscapeKeyDown={(e) => e.preventDefault()} onPointerDownOutside={(e) => e.preventDefault()} onInteractOutside={(e) => e.preventDefault()}>
        {apiKey && (
          <>
            <DialogBody>
              <Callout tone="warn" title="This is the only time you will see the secret">
                The server keeps it encrypted and will not show it again. Copy it to your secret store now.
              </Callout>
              <div className="flex flex-col gap-1">
                <span className="text-xs text-ink-3">API key</span>
                <div className="flex min-w-0 items-center gap-1 rounded-xs border bg-sunken px-2 py-1.5">
                  <code data-testid="reveal-api-key" className="mono min-w-0 flex-1 text-sm break-all">
                    {apiKey.api_key}
                  </code>
                  <CopyButton value={apiKey.api_key} />
                </div>
              </div>
              <div className="flex flex-col gap-1">
                <span className="text-xs text-ink-3">Secret</span>
                <div className="flex min-w-0 items-center gap-1 rounded-xs border border-line-2 bg-sunken px-2 py-1.5">
                  <code data-testid="reveal-secret" className="mono min-w-0 flex-1 text-sm break-all">
                    {apiKey.secret}
                  </code>
                  <CopyButton value={apiKey.secret ?? ""} />
                </div>
              </div>
              <CheckRow label="I have saved the secret somewhere safe" checked={saved} onCheckedChange={setSaved} />
            </DialogBody>
            <DialogFooter>
              <Button variant="primary" disabled={!saved} onClick={onDone}>
                Done
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}

function keyStatus(k: ApiKey, now: number) {
  if (k.revoked_at) return <StatusBadge tone="neutral">Revoked {formatRelative(k.revoked_at, now)}</StatusBadge>;
  if (k.expires_at && Date.parse(k.expires_at) <= now) return <StatusBadge tone="neutral">Expired</StatusBadge>;
  if (k.expires_at) return <StatusBadge tone="warn" title={formatDateTime(k.expires_at)}>Rotating, ends {formatRelative(k.expires_at, now)}</StatusBadge>;
  return <StatusBadge tone="good">Active</StatusBadge>;
}

const OVERLAPS = [
  { value: "0", label: "One day (the default)" },
  { value: "3600", label: "One hour" },
  { value: "604800", label: "Seven days" },
  { value: "2592000", label: "Thirty days" },
  { value: "1", label: "End it right away" },
];

function RotateDialog({ apiKey, onClose, onRotated }: { apiKey: ApiKey | null; onClose: () => void; onRotated: (k: ApiKey) => void }) {
  const call = useProjectApi();
  const { projectId } = useProject();
  const qc = useQueryClient();
  const form = useForm({ defaultValues: { overlap: "0" } });
  const rotate = useMutation<ApiKey, ApiError, number>({
    mutationFn: (overlap_seconds) => call<ApiKey>(`/keys/${apiKey?.id}/rotate`, { method: "POST", body: overlap_seconds ? { overlap_seconds } : {} }),
    onSuccess: async (k) => {
      await qc.invalidateQueries({ queryKey: [projectId, "keys"] });
      onClose();
      onRotated(k);
    },
  });
  return (
    <Dialog open={!!apiKey} onOpenChange={(o) => !o && onClose()}>
      <DialogContent title={`Rotate ${apiKey?.api_key}`} description="You get a new key and secret. The old pair keeps working for the overlap, so running services have time to switch.">
        <form onSubmit={form.handleSubmit((v) => rotate.mutate(Number(v.overlap)))}>
          <DialogBody>
            <Field label="Old key keeps working for">
              <Controller control={form.control} name="overlap" render={({ field }) => <Select value={field.value} onValueChange={field.onChange} options={OVERLAPS} />} />
            </Field>
            {rotate.error && (
              <p role="alert" className="text-sm text-critical-text">
                {rotate.error.message}
              </p>
            )}
          </DialogBody>
          <DialogFooter>
            <Button onClick={onClose}>Cancel</Button>
            <Button type="submit" variant="primary" pending={rotate.isPending}>
              Rotate key
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function ApiKeysPanel() {
  const call = useProjectApi();
  const { projectId } = useProject();
  const qc = useQueryClient();
  const now = useNow(10000);
  const keys = useProjectQuery<Items<ApiKey>>(["keys"], "/keys");
  const [creating, setCreating] = React.useState(false);
  const [name, setName] = React.useState("");
  const [revealed, setRevealed] = React.useState<{ key: ApiKey; heading: string } | null>(null);
  const [rotating, setRotating] = React.useState<ApiKey | null>(null);
  const [revoking, setRevoking] = React.useState<ApiKey | null>(null);
  const [showDead, setShowDead] = React.useState(false);

  const create = useMutation<ApiKey, ApiError>({
    mutationFn: () => call<ApiKey>("/keys", { method: "POST", body: { name } }),
    onSuccess: async (k) => {
      await qc.invalidateQueries({ queryKey: [projectId, "keys"] });
      setCreating(false);
      setName("");
      setRevealed({ key: k, heading: "Your new API key" });
    },
  });

  if (isForbidden(keys.error)) {
    return (
      <Panel>
        <EmptyState icon={KeyRound} title="Only owners and admins can see API keys">
          <p>Your role in this organization is member. Ask an owner or admin for a key, or use the token generator, which is open to members.</p>
        </EmptyState>
      </Panel>
    );
  }

  const all = keys.data?.items ?? [];
  const dead = all.filter((k) => k.revoked_at || (k.expires_at && Date.parse(k.expires_at) <= now));
  const shown = showDead ? all : all.filter((k) => !dead.includes(k));

  return (
    <Panel
      title="API keys"
      description="Your backend uses a key and its secret to sign access tokens. Secrets are shown once."
      actions={
        <Button variant="primary" onClick={() => setCreating(true)}>
          <Plus aria-hidden /> Create API key
        </Button>
      }
    >
      {keys.isPending ? (
        <TableSkeleton rows={2} />
      ) : keys.error ? (
        <ErrorState error={keys.error} onRetry={() => keys.refetch()} />
      ) : all.length === 0 ? (
        <EmptyState icon={KeyRound} title="This project has no API keys">
          <p>You need one before anyone can connect, because every access token is signed with a key. Create one and copy its secret to your backend.</p>
        </EmptyState>
      ) : (
        <>
          {shown.length === 0 ? (
            <EmptyState title="Every key has been revoked or has expired" className="py-6">
              <p>Create a new one to let clients connect again.</p>
            </EmptyState>
          ) : (
            <Table>
              <thead>
                <tr>
                  <Th>API key</Th>
                  <Th className="hidden sm:table-cell">Name</Th>
                  <Th className="hidden md:table-cell">Created</Th>
                  <Th>Status</Th>
                  <Th>
                    <span className="sr-only">Actions</span>
                  </Th>
                </tr>
              </thead>
              <tbody>
                {shown.map((k) => (
                  <Tr key={k.id} data-testid="key-row" data-key={k.api_key}>
                    <Td className="max-w-52">
                      <CopyValue value={k.api_key} className="text-xs" />
                    </Td>
                    <Td className="hidden max-w-40 truncate sm:table-cell">{k.name || <span className="text-ink-3">Unnamed</span>}</Td>
                    <Td className="hidden whitespace-nowrap text-ink-2 md:table-cell" title={formatDateTime(k.created_at)}>
                      {formatShortTime(k.created_at)}
                    </Td>
                    <Td>{keyStatus(k, now)}</Td>
                    <Td className="text-right whitespace-nowrap">
                      {!dead.includes(k) && (
                        <span className="inline-flex flex-col gap-1 sm:flex-row">
                          <Button size="sm" onClick={() => setRotating(k)}>
                            Rotate
                          </Button>
                          <Button size="sm" variant="danger" onClick={() => setRevoking(k)}>
                            Revoke
                          </Button>
                        </span>
                      )}
                    </Td>
                  </Tr>
                ))}
              </tbody>
            </Table>
          )}
          {dead.length > 0 && (
            <div className="px-3 py-2">
              <Button variant="link" size="sm" className="px-0" onClick={() => setShowDead((v) => !v)}>
                {showDead ? "Hide" : "Show"} {dead.length} revoked or expired {dead.length === 1 ? "key" : "keys"}
              </Button>
            </div>
          )}
        </>
      )}

      <Dialog open={creating} onOpenChange={setCreating}>
        <DialogContent title="Create an API key">
          <form
            onSubmit={(e) => {
              e.preventDefault();
              create.mutate();
            }}
          >
            <DialogBody>
              <Field label="Name" optional hint="To remember what uses it, like production-backend." error={create.error?.fields?.name}>
                <Input autoFocus value={name} onChange={(e) => setName(e.target.value)} />
              </Field>
              {create.error && !create.error.fields && (
                <p role="alert" className="text-sm text-critical-text">
                  {create.error.message}
                </p>
              )}
            </DialogBody>
            <DialogFooter>
              <Button onClick={() => setCreating(false)}>Cancel</Button>
              <Button type="submit" variant="primary" pending={create.isPending}>
                Create key
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <RotateDialog apiKey={rotating} onClose={() => setRotating(null)} onRotated={(key) => setRevealed({ key, heading: "Your replacement API key" })} />
      <SecretReveal apiKey={revealed?.key ?? null} heading={revealed?.heading ?? ""} onDone={() => setRevealed(null)} />
      <ConfirmDialog
        open={!!revoking}
        onOpenChange={(o) => !o && setRevoking(null)}
        title={`Revoke ${revoking?.api_key}?`}
        description="It stops working within 30 seconds on every node, and tokens signed with it stop verifying. Webhooks that sign with it stop delivering. This cannot be undone."
        confirmLabel="Revoke key"
        onConfirm={async () => {
          await call(`/keys/${revoking?.id}`, { method: "DELETE" });
          await qc.invalidateQueries({ queryKey: [projectId, "keys"] });
          toast("Key revoked");
        }}
      />
    </Panel>
  );
}
