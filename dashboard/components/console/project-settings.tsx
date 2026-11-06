"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Plus, X } from "lucide-react";
import * as React from "react";
import { Controller, useFieldArray, useForm } from "react-hook-form";
import { Callout } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { toast } from "@/components/ui/toast";
import { api, ApiError } from "@/lib/api";
import { optionalNumber } from "@/lib/form";
import { meKey, useProject, useUpdateSettings } from "@/lib/session";
import type { IceServer, Project } from "@/lib/types";
import { Panel } from "./page";

function SaveRow({ pending, disabled, error }: { pending: boolean; disabled: boolean; error?: string | null }) {
  return (
    <div className="flex flex-wrap items-center gap-3">
      <Button type="submit" variant="primary" pending={pending} disabled={disabled}>
        Save
      </Button>
      {error && (
        <p role="alert" className="text-sm text-critical-text">
          {error}
        </p>
      )}
    </div>
  );
}

function serverMessage(err: ApiError | null): string | null {
  if (!err) return null;
  const field = err.fields && Object.values(err.fields)[0];
  return field ? field.charAt(0).toUpperCase() + field.slice(1) + "." : err.message;
}

function OriginsPanel() {
  const { project, canAdmin } = useProject();
  const update = useUpdateSettings();
  const saved = (project.settings.allowed_origins ?? []).join("\n");
  const [value, setValue] = React.useState(saved);
  React.useEffect(() => setValue(saved), [saved]);
  return (
    <Panel title="Allowed origins" description="Which web pages may connect to this project's rooms.">
      <form
        className="flex flex-col gap-3 p-3"
        onSubmit={(e) => {
          e.preventDefault();
          const allowed_origins = value.split("\n").map((s) => s.trim()).filter(Boolean);
          update.mutate({ allowed_origins }, { onSuccess: () => toast("Allowed origins saved") });
        }}
      >
        <Field label="Origins, one per line" hint="Like https://app.example.com. Leave empty to allow any page, because tokens are what authenticate clients.">
          <Textarea rows={3} className="font-mono text-xs" disabled={!canAdmin} value={value} onChange={(e) => setValue(e.target.value)} placeholder="https://app.example.com" />
        </Field>
        <SaveRow pending={update.isPending} disabled={!canAdmin || value === saved} error={serverMessage(update.error)} />
      </form>
    </Panel>
  );
}

function RoomDefaultsPanel() {
  const { project, canAdmin } = useProject();
  const update = useUpdateSettings();
  const s = project.settings;
  const values = {
    empty_timeout: s.empty_timeout?.toString() ?? "",
    departure_timeout: s.departure_timeout?.toString() ?? "",
    max_participants: s.max_participants?.toString() ?? "",
    auto_create: s.auto_create_rooms === undefined ? "server" : s.auto_create_rooms ? "on" : "off",
  };
  const form = useForm({ values });
  return (
    <Panel title="Room defaults" description="Applied to rooms created in this project, unless a request says otherwise.">
      <form
        className="flex flex-col gap-3 p-3"
        onSubmit={form.handleSubmit((v) =>
          update.mutate(
            {
              empty_timeout: optionalNumber(v.empty_timeout),
              departure_timeout: optionalNumber(v.departure_timeout),
              max_participants: optionalNumber(v.max_participants),
              auto_create_rooms: v.auto_create === "server" ? undefined : v.auto_create === "on",
            },
            { onSuccess: () => toast("Room defaults saved") },
          ),
        )}
      >
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="Empty timeout" hint="Seconds a room nobody has joined stays open. Blank uses the server's default, 300 unless configured otherwise.">
            <Input type="number" min={0} max={604800} disabled={!canAdmin} {...form.register("empty_timeout")} />
          </Field>
          <Field label="Departure timeout" hint="Seconds a room stays open after the last person leaves. Blank uses the server's default, 20 unless configured otherwise.">
            <Input type="number" min={0} max={604800} disabled={!canAdmin} {...form.register("departure_timeout")} />
          </Field>
          <Field label="Max participants" hint="Blank means no limit.">
            <Input type="number" min={0} disabled={!canAdmin} {...form.register("max_participants")} />
          </Field>
          <Field label="Open rooms when someone joins" hint="If off, a room must be created first, or joining fails.">
            <Controller
              control={form.control}
              name="auto_create"
              render={({ field }) => (
                <Select
                  disabled={!canAdmin}
                  value={field.value}
                  onValueChange={field.onChange}
                  options={[
                    { value: "server", label: "Use the server's setting" },
                    { value: "on", label: "On" },
                    { value: "off", label: "Off" },
                  ]}
                />
              )}
            />
          </Field>
        </div>
        <SaveRow pending={update.isPending} disabled={!canAdmin || !form.formState.isDirty} error={serverMessage(update.error)} />
      </form>
    </Panel>
  );
}

function TokenSettingsPanel() {
  const { project, canAdmin } = useProject();
  const update = useUpdateSettings();
  const form = useForm({ values: { ttl: project.settings.token_ttl_seconds?.toString() ?? "" } });
  return (
    <Panel title="Token settings" description="For tokens minted by this dashboard and POST /tokens. Tokens your backend signs itself set their own lifetime.">
      <form
        className="flex flex-col gap-3 p-3"
        onSubmit={form.handleSubmit((v) => update.mutate({ token_ttl_seconds: optionalNumber(v.ttl) }, { onSuccess: () => toast("Token settings saved") }))}
      >
        <Field label="Default lifetime in seconds" hint="Blank means 6 hours. At most 30 days (2592000).">
          <Input type="number" min={0} max={2592000} disabled={!canAdmin} className="max-w-48" {...form.register("ttl")} />
        </Field>
        <SaveRow pending={update.isPending} disabled={!canAdmin || !form.formState.isDirty} error={serverMessage(update.error)} />
      </form>
    </Panel>
  );
}

interface IceRow {
  urls: string;
  username: string;
  credential: string;
}

function IceServersPanel() {
  const { project, canAdmin } = useProject();
  const update = useUpdateSettings();
  const toRows = (servers: IceServer[] | undefined): IceRow[] => (servers ?? []).map((s) => ({ urls: s.urls.join(", "), username: s.username ?? "", credential: s.credential ?? "" }));
  const form = useForm<{ servers: IceRow[] }>({ values: { servers: toRows(project.settings.ice_servers) } });
  const { fields, append, remove } = useFieldArray({ control: form.control, name: "servers" });
  return (
    <Panel title="TURN and ICE servers" description="Handed to clients when they join. Clients behind strict firewalls need a TURN server to connect at all.">
      <form
        className="flex flex-col gap-3 p-3"
        onSubmit={form.handleSubmit((v) => {
          const ice_servers: IceServer[] = v.servers.map((r) => ({
            urls: r.urls.split(",").map((u) => u.trim()).filter(Boolean),
            username: r.username || undefined,
            credential: r.credential || undefined,
          }));
          update.mutate({ ice_servers }, { onSuccess: () => toast("ICE servers saved") });
        })}
      >
        {fields.length === 0 && <p className="text-sm text-ink-3">None configured. Clients use only this server&rsquo;s own candidates.</p>}
        {fields.map((f, i) => (
          <div key={f.id} className="grid items-start gap-2 border-b pb-3 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)_minmax(0,1fr)_auto]" data-testid="ice-row">
            <Field label="URLs" hint="stun:, turn: or turns:, separated by commas.">
              <Input disabled={!canAdmin} placeholder="turn:turn.example.com:3478" {...form.register(`servers.${i}.urls`, { required: true })} />
            </Field>
            <Field label="Username" optional>
              <Input disabled={!canAdmin} autoComplete="off" {...form.register(`servers.${i}.username`)} />
            </Field>
            <Field label="Credential" optional>
              <Input disabled={!canAdmin} autoComplete="off" {...form.register(`servers.${i}.credential`)} />
            </Field>
            <Button variant="ghost" size="icon" className="mt-5" disabled={!canAdmin} aria-label={`Remove server ${i + 1}`} onClick={() => remove(i)}>
              <X aria-hidden />
            </Button>
          </div>
        ))}
        <div>
          <Button size="sm" disabled={!canAdmin} onClick={() => append({ urls: "", username: "", credential: "" })}>
            <Plus aria-hidden /> Add a server
          </Button>
        </div>
        <SaveRow pending={update.isPending} disabled={!canAdmin || !form.formState.isDirty} error={serverMessage(update.error)} />
      </form>
    </Panel>
  );
}

function RegionPanel() {
  const { project, canAdmin } = useProject();
  const qc = useQueryClient();
  const [value, setValue] = React.useState(project.region);
  React.useEffect(() => setValue(project.region), [project.region]);
  const save = useMutation<Project, ApiError>({
    mutationFn: () => api<Project>(`/projects/${project.id}`, { method: "PATCH", body: { region: value } }),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: meKey });
      toast("Region saved");
    },
  });
  return (
    <Panel title="Region" description="A label for your own reference. It does not change where media is routed yet.">
      <form
        className="flex flex-col gap-3 p-3"
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <Field label="Region" error={save.error?.fields?.region}>
          <Input className="max-w-48" disabled={!canAdmin} value={value} onChange={(e) => setValue(e.target.value)} />
        </Field>
        <SaveRow pending={save.isPending} disabled={!canAdmin || value === project.region || !value.trim()} error={save.error && !save.error.fields ? save.error.message : null} />
      </form>
    </Panel>
  );
}

export function ProjectSettingsPanels() {
  const { canAdmin } = useProject();
  return (
    <div className="flex flex-col gap-4">
      {!canAdmin && <Callout title="You can look, but not change these">Your role in this organization is member. Owners and admins can edit project settings.</Callout>}
      <p className="text-sm text-ink-2">Other nodes pick up a change within 30 seconds. Rooms already open keep the settings they were created with.</p>
      <div className="grid items-start gap-4 xl:grid-cols-2">
        <RoomDefaultsPanel />
        <div className="flex flex-col gap-4">
          <OriginsPanel />
          <TokenSettingsPanel />
          <RegionPanel />
        </div>
      </div>
      <IceServersPanel />
    </div>
  );
}
