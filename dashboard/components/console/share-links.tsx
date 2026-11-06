"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ExternalLink, Link2 } from "lucide-react";
import * as React from "react";
import { useForm, Controller } from "react-hook-form";
import { Callout } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { CopyButton } from "@/components/ui/copy";
import { Dialog, DialogBody, DialogContent, DialogFooter } from "@/components/ui/dialog";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { toast } from "@/components/ui/toast";
import { ApiError } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { applyApiError } from "@/lib/form";
import { useProject, useProjectApi, useProjectQuery } from "@/lib/session";
import type { Items, JoinLink } from "@/lib/types";
import { EmptyState, ErrorState, Panel } from "./page";

const EXPIRY = [
  { value: "3600", label: "1 hour" },
  { value: "86400", label: "1 day" },
  { value: "604800", label: "7 days" },
  { value: "2592000", label: "30 days" },
];

const ACCESS = [
  { value: "talk", label: "Talk and share video" },
  { value: "watch", label: "Watch and listen only" },
  { value: "host", label: "Run the meeting, as a host" },
];

interface LinkForm {
  room: string;
  label: string;
  expires: string;
  access: string;
}

function isLocalOnly(url: string): boolean {
  try {
    const host = new URL(url).hostname;
    return host === "localhost" || host === "127.0.0.1" || host === "[::1]";
  } catch {
    return false;
  }
}

/** A link that works only on this computer looks just like one that works everywhere, so this says so. */
function LocalOnlyNote() {
  return (
    <Callout tone="serious" title="This link only works on this computer">
      Its address comes from <code>public_dashboard_url</code> in the server&rsquo;s configuration. To share it with other people, set <code>public_dashboard_url</code> and <code>public_ws_url</code> to addresses they can reach. Use HTTPS: browsers only allow cameras and microphones on secure pages.
    </Callout>
  );
}

function LinkRow({ link }: { link: JoinLink }) {
  const call = useProjectApi();
  const { projectId } = useProject();
  const qc = useQueryClient();
  const [confirm, setConfirm] = React.useState(false);
  return (
    <li data-testid="share-link" className="flex flex-col gap-2 border-t p-3 first:border-t-0">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="min-w-0">
          <div className="truncate text-sm font-medium">{link.label || link.room}</div>
          <div className="text-xs text-ink-3">
            {link.role === "host" ? "Host link: whoever opens it runs the meeting" : link.can_publish ? "Can talk and share video" : "Watch and listen only"}. Expires {formatDateTime(link.expires_at)}. Used <span data-testid="share-link-uses">{link.uses}</span> {link.uses === 1 ? "time" : "times"}.
          </div>
        </div>
        <div className="flex items-center gap-1">
          <CopyButton value={link.url} label="Copy link" variant="secondary" />
          <Button size="sm" asChild>
            <a href={link.url} target="_blank" rel="noopener noreferrer">
              <ExternalLink aria-hidden /> Open
            </a>
          </Button>
          <Button size="sm" variant="danger" data-testid="share-link-revoke" onClick={() => setConfirm(true)}>
            Turn off
          </Button>
        </div>
      </div>
      <input readOnly aria-label="Link" data-testid="share-link-url" value={link.url} onFocus={(e) => e.currentTarget.select()} className="mono w-full min-w-0 truncate rounded-sm border bg-sunken px-2 py-1 text-xs" />
      <ConfirmDialog
        open={confirm}
        onOpenChange={setConfirm}
        destructive
        title="Turn this link off?"
        description="Nobody new can join with it, and it cannot be turned back on. People already in the room stay until they leave or you remove them."
        confirmLabel="Turn off"
        onConfirm={async () => {
          await call(`/join-links/${link.id}`, { method: "DELETE" });
          await qc.invalidateQueries({ queryKey: [projectId, "join-links"] });
          toast("Link turned off");
        }}
      />
    </li>
  );
}

/** Links that work now, optionally for one room. */
export function ShareLinksList({ room, onCreate }: { room?: string; onCreate?: () => void }) {
  const links = useProjectQuery<Items<JoinLink>>(["join-links", room ?? ""], `/join-links${room ? `?room=${encodeURIComponent(room)}` : ""}`);
  if (links.isPending) {
    return (
      <div className="p-3">
        <Skeleton className="h-16" />
      </div>
    );
  }
  if (links.error) return <ErrorState error={links.error} onRetry={() => links.refetch()} />;
  if (links.data.items.length === 0) {
    return (
      <EmptyState
        icon={Link2}
        title="No share links"
        action={
          onCreate && (
            <Button variant="primary" data-testid="share-links-empty-create" onClick={onCreate}>
              <Link2 aria-hidden /> Create a link
            </Button>
          )
        }
      >
        <p>A share link lets anyone who has it join without an account.</p>
      </EmptyState>
    );
  }
  return (
    <>
      {links.data.items.some((l) => isLocalOnly(l.url)) && (
        <div className="p-3 pb-0">
          <LocalOnlyNote />
        </div>
      )}
      <ul>
        {links.data.items.map((l) => (
          <LinkRow key={l.id} link={l} />
        ))}
      </ul>
    </>
  );
}

export function ShareRoomDialog({ open, onOpenChange, room }: { open: boolean; onOpenChange: (open: boolean) => void; room?: string }) {
  const call = useProjectApi();
  const { projectId } = useProject();
  const qc = useQueryClient();
  const form = useForm<LinkForm>({ defaultValues: { room: room ?? "", label: "", expires: "86400", access: "talk" } });
  const [error, setError] = React.useState<string | null>(null);
  const create = useMutation<JoinLink, ApiError, LinkForm>({
    mutationFn: (v) =>
      call<JoinLink>("/join-links", {
        method: "POST",
        body: { room: v.room, label: v.label || undefined, expires_in_seconds: Number(v.expires), can_publish: v.access !== "watch", role: v.access === "host" ? "host" : "guest" },
      }),
    onSuccess: () => {
      setError(null);
      void qc.invalidateQueries({ queryKey: [projectId, "join-links"] });
    },
    onError: (err) => setError(err.code === "no_api_key" ? "This project has no API key to sign with. Create one under Developer, then try again." : applyApiError(form, err)),
  });
  const { reset: resetForm } = form;
  const { reset: resetCreate } = create;

  React.useEffect(() => {
    if (open) {
      resetForm({ room: room ?? "", label: "", expires: "86400", access: "talk" });
      resetCreate();
      setError(null);
    }
  }, [open, room, resetForm, resetCreate]);

  const { errors } = form.formState;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent title="Share a link to this room" description="Anyone with the link can join from their browser, with no account. The room opens when the first person arrives.">
        {create.data ? (
          <>
            <DialogBody>
              <Field label="Your link">
                <Input readOnly data-testid="new-share-link" value={create.data.url} className="mono text-xs" onFocus={(e) => e.currentTarget.select()} />
              </Field>
              {isLocalOnly(create.data.url) && <LocalOnlyNote />}
              <p className="text-sm text-ink-2">Treat it like a key: anyone you send it to can pass it on. You can turn it off at any time. It expires {formatDateTime(create.data.expires_at)}.</p>
            </DialogBody>
            <DialogFooter>
              <CopyButton value={create.data.url} label="Copy link" variant="primary" />
              <Button onClick={() => onOpenChange(false)}>Done</Button>
            </DialogFooter>
          </>
        ) : (
          <form onSubmit={form.handleSubmit((v) => create.mutate(v))}>
            <DialogBody>
              <Field label="Room" error={errors.room?.message}>
                <Input autoFocus={!room} readOnly={!!room} placeholder="standup" data-testid="share-room" {...form.register("room", { required: "Say which room it joins." })} />
              </Field>
              <Field label="Title" optional hint="What people see before they join. By default, the room's name." error={errors.label?.message}>
                <Input maxLength={100} data-testid="share-label" {...form.register("label")} />
              </Field>
              <div className="grid gap-3 sm:grid-cols-2">
                <Field label="People can">
                  <Controller control={form.control} name="access" render={({ field }) => <Select aria-label="People can" value={field.value} onValueChange={field.onChange} options={ACCESS} />} />
                </Field>
                <Field label="Expires after">
                  <Controller control={form.control} name="expires" render={({ field }) => <Select value={field.value} onValueChange={field.onChange} options={EXPIRY} />} />
                </Field>
              </div>
              {error && (
                <p role="alert" className="text-sm text-critical-text">
                  {error}
                </p>
              )}
            </DialogBody>
            <DialogFooter>
              <Button onClick={() => onOpenChange(false)}>Cancel</Button>
              <Button type="submit" variant="primary" pending={create.isPending} data-testid="share-create">
                Create link
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}

export function ShareLinksPanel({ room, onCreate }: { room: string; onCreate: () => void }) {
  return (
    <Panel title="Share links" description="Anyone with one of these can join this room without an account.">
      <ShareLinksList room={room} onCreate={onCreate} />
    </Panel>
  );
}
