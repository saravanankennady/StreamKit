"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft } from "lucide-react";
import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import * as React from "react";
import { Chart } from "@/components/charts/chart";
import { EmptyState, ErrorState, Facts, Page, Panel } from "@/components/console/page";
import { RemoveParticipant, TracksTable, UnmuteNote } from "@/components/console/participants";
import { ParticipantStateBadge, QualityBadge } from "@/components/console/status";
import { NotMeasured } from "@/components/console/tile";
import { Button } from "@/components/ui/button";
import { CopyValue } from "@/components/ui/copy";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Skeleton, TableSkeleton } from "@/components/ui/skeleton";
import { CheckRow } from "@/components/ui/switch";
import { toast } from "@/components/ui/toast";
import { ApiError } from "@/lib/api";
import { formatBitrate, formatBitrateTick, formatClock, formatDateTime, formatDecimal, formatDuration, formatMsTick } from "@/lib/format";
import { useNow } from "@/lib/hooks";
import { rollingDomain, useLive } from "@/lib/live";
import { useProject, useProjectApi, useProjectQuery } from "@/lib/session";
import { sourceNames, TRACK_SOURCES } from "@/lib/track-source";
import type { ParticipantStat } from "@/lib/types";

interface PermissionForm {
  can_publish: boolean;
  can_subscribe: boolean;
  can_publish_data: boolean;
  can_update_metadata: boolean;
  hidden: boolean;
  sources: string[];
}

function toForm(p: ParticipantStat): PermissionForm {
  const x = p.permission ?? {};
  return {
    can_publish: !!x.can_publish,
    can_subscribe: !!x.can_subscribe,
    can_publish_data: !!x.can_publish_data,
    can_update_metadata: !!x.can_update_metadata,
    hidden: !!x.hidden,
    sources: sourceNames(x.can_publish_sources),
  };
}

function PermissionsPanel({ participant }: { participant: ParticipantStat }) {
  const call = useProjectApi();
  const { projectId } = useProject();
  const qc = useQueryClient();
  const [form, setForm] = React.useState<PermissionForm>(() => toForm(participant));
  // Follow the server until someone starts editing. Polling must not undo a click.
  const [dirty, setDirty] = React.useState(false);
  const serverJson = JSON.stringify(toForm(participant));
  React.useEffect(() => {
    if (!dirty) setForm(JSON.parse(serverJson) as PermissionForm);
  }, [serverJson, dirty]);
  const edit = (patch: Partial<PermissionForm>) => {
    setDirty(true);
    setForm((f) => ({ ...f, ...patch }));
  };
  const save = useMutation<unknown, ApiError>({
    mutationFn: () =>
      call(`/participants/${encodeURIComponent(participant.room)}/${encodeURIComponent(participant.identity)}`, {
        method: "PATCH",
        body: {
          permission: {
            can_publish: form.can_publish,
            can_subscribe: form.can_subscribe,
            can_publish_data: form.can_publish_data,
            can_update_metadata: form.can_update_metadata,
            hidden: form.hidden,
            can_publish_sources: form.can_publish ? form.sources : [],
          },
        },
      }),
    onSuccess: async () => {
      // The figures this page polls can be a second old, so the form waits before following them again.
      setTimeout(() => setDirty(false), 1500);
      await qc.invalidateQueries({ queryKey: [projectId, "participant", participant.room, participant.identity] });
      toast("Permissions updated", `${participant.identity} was told. Tracks they may no longer publish stop.`);
    },
  });
  return (
    <Panel title="Permissions" description="Saving replaces the whole set. Takes effect at once.">
      <form
        className="flex flex-col gap-3 p-3"
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <div className="grid gap-2.5 sm:grid-cols-2">
          <CheckRow label="Publish media" checked={form.can_publish} onCheckedChange={(v) => edit({ can_publish: v })} />
          <CheckRow label="Subscribe to others" checked={form.can_subscribe} onCheckedChange={(v) => edit({ can_subscribe: v })} />
          <CheckRow label="Send data messages" checked={form.can_publish_data} onCheckedChange={(v) => edit({ can_publish_data: v })} />
          <CheckRow label="Change their own metadata" checked={form.can_update_metadata} onCheckedChange={(v) => edit({ can_update_metadata: v })} />
          <CheckRow label="Hidden" hint="Others do not see them in the room." checked={form.hidden} onCheckedChange={(v) => edit({ hidden: v })} />
        </div>
        <fieldset disabled={!form.can_publish} className="disabled:opacity-50">
          <legend className="text-sm font-medium">Sources they may publish</legend>
          <p className="mb-2 text-xs text-ink-3">Tick none to allow every source.</p>
          <div className="grid gap-2 sm:grid-cols-2">
            {TRACK_SOURCES.map((s) => (
              <CheckRow
                key={s.value}
                label={s.label}
                disabled={!form.can_publish}
                checked={form.sources.includes(s.value)}
                onCheckedChange={(v) => edit({ sources: v ? [...form.sources, s.value] : form.sources.filter((x) => x !== s.value) })}
              />
            ))}
          </div>
        </fieldset>
        {save.error && (
          <p role="alert" className="text-sm text-critical-text">
            {save.error.message}
          </p>
        )}
        <div>
          <Button type="submit" variant="primary" pending={save.isPending} disabled={!dirty}>
            Save permissions
          </Button>
        </div>
      </form>
    </Panel>
  );
}

function ProfilePanel({ participant }: { participant: ParticipantStat }) {
  const call = useProjectApi();
  const [name, setName] = React.useState(participant.name);
  const [metadata, setMetadata] = React.useState("");
  const [attributes, setAttributes] = React.useState("");
  const [attrError, setAttrError] = React.useState<string | null>(null);
  const save = useMutation<{ metadata?: string; name?: string }, ApiError, Record<string, unknown>>({
    mutationFn: (body) => call(`/participants/${encodeURIComponent(participant.room)}/${encodeURIComponent(participant.identity)}`, { method: "PATCH", body }),
    onSuccess: () => toast("Participant updated"),
  });
  return (
    <Panel title="Name, metadata and attributes" description="The API does not report current metadata or attributes here, so those fields start empty. Fill in only what you want to replace.">
      <form
        className="flex flex-col gap-3 p-3"
        onSubmit={(e) => {
          e.preventDefault();
          setAttrError(null);
          const body: Record<string, unknown> = {};
          if (name !== participant.name) body.name = name;
          if (metadata) body.metadata = metadata;
          if (attributes.trim()) {
            try {
              const parsed: unknown = JSON.parse(attributes);
              if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed) || Object.values(parsed).some((v) => typeof v !== "string")) throw new Error();
              body.attributes = parsed;
            } catch {
              setAttrError('Attributes must be a JSON object of strings, like {"role": "host"}.');
              return;
            }
          }
          if (Object.keys(body).length > 0) save.mutate(body);
        }}
      >
        <Field label="Display name">
          <Input value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="Metadata" optional>
          <Textarea rows={2} className="min-h-14 font-mono text-xs" value={metadata} onChange={(e) => setMetadata(e.target.value)} />
        </Field>
        <Field label="Attributes" optional hint='A JSON object of strings, like {"role": "host"}.' error={attrError ?? undefined}>
          <Textarea rows={2} className="min-h-14 font-mono text-xs" value={attributes} onChange={(e) => setAttributes(e.target.value)} />
        </Field>
        {save.error && (
          <p role="alert" className="text-sm text-critical-text">
            {save.error.message}
          </p>
        )}
        <div>
          <Button type="submit" pending={save.isPending}>
            Save changes
          </Button>
        </div>
      </form>
    </Panel>
  );
}

export default function ParticipantPage() {
  const params = useParams<{ room: string; identity: string }>();
  const room = decodeURIComponent(params.room);
  const identity = decodeURIComponent(params.identity);
  const router = useRouter();
  const now = useNow(1000);
  const query = useProjectQuery<ParticipantStat>(["participant", room, identity], `/participants/${encodeURIComponent(room)}/${encodeURIComponent(identity)}`, {
    refetchInterval: 1500,
  });
  const p = query.data;
  const samples = useLive((s) => (p ? s.participantHistory[p.sid] : undefined)) ?? [];
  const domain = rollingDomain(samples);

  const back = (
    <Link href="/participants" className="link inline-flex items-center gap-1 text-sm text-ink-2">
      <ArrowLeft className="size-3" aria-hidden /> All participants
    </Link>
  );

  if (query.error) {
    if (query.error.status === 404) {
      return (
        <Page title={identity} description={back}>
          <Panel>
            <EmptyState
              title={`${identity} is not in ${room}`}
              action={
                <Button asChild>
                  <Link href="/participants">See who is connected</Link>
                </Button>
              }
            >
              <p>They left, or the room closed. The event log has what happened.</p>
              <p>
                <Link className="link" href={`/logs?q=${encodeURIComponent(identity)}`}>
                  Search the log for {identity}
                </Link>
              </p>
            </EmptyState>
          </Panel>
        </Page>
      );
    }
    return (
      <Page title={identity} description={back}>
        <Panel>
          <ErrorState error={query.error} onRetry={() => query.refetch()} />
        </Panel>
      </Page>
    );
  }

  return (
    <Page
      title={
        <span className="flex flex-wrap items-center gap-2">
          {identity} {p && <ParticipantStateBadge state={p.state} />}
        </span>
      }
      description={back}
      actions={p && <RemoveParticipant participant={p} label="Remove from room" onRemoved={() => router.push("/participants")} />}
    >
      <Panel title="Connection">
        {!p ? (
          <div className="p-3">
            <Skeleton className="h-16" />
          </div>
        ) : (
          <Facts
            items={[
              { label: "SID", value: <CopyValue value={p.sid} /> },
              {
                label: "Room",
                value: (
                  <Link className="link" href={`/rooms/${encodeURIComponent(p.room)}`}>
                    {p.room}
                  </Link>
                ),
              },
              { label: "Name", value: p.name || "–" },
              { label: "Kind", value: <span className="capitalize">{p.kind.toLowerCase()}</span> },
              { label: "Network quality", value: <QualityBadge quality={p.quality} /> },
              { label: "Joined", value: formatDateTime(p.joined_at) },
              { label: "In the room for", value: formatDuration(Math.max(0, now / 1000 - p.joined_at)) },
              { label: "Subscribed to", value: `${p.subscribed_count} ${p.subscribed_count === 1 ? "track" : "tracks"}` },
              { label: "Bitrate in", value: formatBitrate(p.bitrate_in), testId: "detail-bitrate-in" },
              { label: "Bitrate out", value: formatBitrate(p.bitrate_out) },
              { label: "Bandwidth estimate", value: p.bandwidth_estimate ? formatBitrate(p.bandwidth_estimate) : <NotMeasured /> },
              { label: "RTT", value: p.rtt_ms > 0 ? `${formatDecimal(p.rtt_ms)} ms` : <NotMeasured>Needs a subscription</NotMeasured> },
              { label: "Jitter", value: p.jitter_ms > 0 ? `${formatDecimal(p.jitter_ms)} ms` : <NotMeasured>Needs a subscription</NotMeasured> },
              { label: "Packets in", value: p.packets_in.toLocaleString("en") },
              { label: "Packets out", value: p.packets_out.toLocaleString("en") },
            ]}
          />
        )}
      </Panel>
      <Panel title="Published tracks">
        {!p ? (
          <TableSkeleton rows={2} />
        ) : p.tracks.length === 0 ? (
          <EmptyState title="No tracks published">
            <p>Connected, but not sending audio or video.</p>
          </EmptyState>
        ) : (
          <TracksTable participant={p} tracks={p.tracks} />
        )}
      </Panel>
      {p && p.tracks.length > 0 && <UnmuteNote />}
      {samples.length > 1 && (
        <Panel title="Last few minutes" description="Kept in this browser from when you opened the console.">
          <div className="grid gap-px bg-line md:grid-cols-2">
            <Chart
              title="Bitrate"
              data={samples.map((s) => ({ x: s.t, in: s.bitrate_in, out: s.bitrate_out }))}
              series={[
                { key: "in", label: "In", slot: 1 },
                { key: "out", label: "Out", slot: 2 },
              ]}
              domain={domain}
              formatX={(x) => formatClock(new Date(x))}
              formatY={formatBitrateTick}
              formatValue={formatBitrate}
            />
            <Chart
              title="RTT and jitter"
              description="Measured on media this participant receives."
              data={samples.map((s) => ({ x: s.t, rtt: s.rtt_ms, jitter: s.jitter_ms }))}
              series={[
                { key: "rtt", label: "RTT", slot: 1 },
                { key: "jitter", label: "Jitter", slot: 2 },
              ]}
              domain={domain}
              formatX={(x) => formatClock(new Date(x))}
              formatY={formatMsTick}
              formatValue={(v) => `${formatDecimal(v)} ms`}
              empty="Not measured until they subscribe to someone."
            />
          </div>
        </Panel>
      )}
      {p && (
        <div className="grid gap-4 xl:grid-cols-2">
          <PermissionsPanel participant={p} />
          <ProfilePanel participant={p} />
        </div>
      )}
    </Page>
  );
}
