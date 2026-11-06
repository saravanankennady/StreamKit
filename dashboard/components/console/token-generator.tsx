"use client";

import { useMutation } from "@tanstack/react-query";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import * as React from "react";
import { Controller, useForm } from "react-hook-form";
import { Callout } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CodeBlock } from "@/components/ui/copy";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { CheckRow } from "@/components/ui/switch";
import { ApiError, decodeJwtClaims } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { applyApiError, optionalNumber } from "@/lib/form";
import { useProject, useProjectApi } from "@/lib/session";
import { TRACK_SOURCES } from "@/lib/track-source";
import { Panel } from "./page";

interface TokenForm {
  identity: string;
  room: string;
  name: string;
  ttl_seconds: string;
  metadata: string;
  attributes: string;
  can_publish: boolean;
  can_subscribe: boolean;
  can_publish_data: boolean;
  can_update_own_metadata: boolean;
  room_admin: boolean;
  room_create: boolean;
  room_list: boolean;
  room_record: boolean;
  hidden: boolean;
  sources: string[];
}

interface TokenResult {
  token: string;
  url: string;
  expires_at: string;
}

const GRANTS: { name: keyof TokenForm; label: string; hint: string }[] = [
  { name: "can_publish", label: "Publish media", hint: "Send audio and video." },
  { name: "can_subscribe", label: "Subscribe", hint: "Receive other people's media." },
  { name: "can_publish_data", label: "Send data", hint: "Data messages to the room." },
  { name: "can_update_own_metadata", label: "Change own metadata", hint: "Name, metadata and attributes." },
  { name: "hidden", label: "Hidden", hint: "Others do not see them in the room." },
];

const ADMIN_GRANTS: { name: keyof TokenForm; label: string; hint: string }[] = [
  { name: "room_create", label: "Create and close rooms", hint: "POST and DELETE /rooms." },
  { name: "room_list", label: "List rooms", hint: "Read rooms, analytics, the monitor and logs." },
  { name: "room_admin", label: "Administer this room", hint: "Manage its participants and tracks." },
  { name: "room_record", label: "Record", hint: "Start, list and stop recordings and egress." },
];

export function TokenGenerator() {
  const call = useProjectApi();
  const { project } = useProject();
  const params = useSearchParams();
  const form = useForm<TokenForm>({
    defaultValues: {
      identity: "",
      room: params.get("room") ?? "",
      name: "",
      ttl_seconds: "",
      metadata: "",
      attributes: "",
      can_publish: true,
      can_subscribe: true,
      can_publish_data: true,
      can_update_own_metadata: false,
      room_admin: false,
      room_create: false,
      room_list: false,
      room_record: false,
      hidden: false,
      sources: [],
    },
  });
  const [error, setError] = React.useState<string | null>(null);
  const [noKey, setNoKey] = React.useState(false);
  const mint = useMutation<TokenResult, ApiError, Record<string, unknown>>({
    mutationFn: (body) => call<TokenResult>("/tokens", { method: "POST", body }),
    onSuccess: () => {
      setError(null);
      setNoKey(false);
    },
    onError: (err) => {
      setNoKey(err.code === "no_api_key");
      setError(err.code === "no_api_key" ? null : applyApiError(form, err));
    },
  });

  const submit = form.handleSubmit((v) => {
    let attributes: unknown;
    if (v.attributes.trim()) {
      try {
        attributes = JSON.parse(v.attributes);
        if (typeof attributes !== "object" || attributes === null || Array.isArray(attributes) || Object.values(attributes).some((x) => typeof x !== "string")) throw new Error();
      } catch {
        form.setError("attributes", { message: 'Use a JSON object of strings, like {"role": "host"}.' });
        return;
      }
    }
    mint.mutate({
      identity: v.identity,
      room: v.room,
      name: v.name || undefined,
      ttl_seconds: optionalNumber(v.ttl_seconds),
      metadata: v.metadata || undefined,
      attributes,
      can_publish: v.can_publish,
      can_subscribe: v.can_subscribe,
      can_publish_data: v.can_publish_data,
      can_update_own_metadata: v.can_update_own_metadata,
      can_publish_sources: v.can_publish && v.sources.length > 0 ? v.sources : undefined,
      room_admin: v.room_admin,
      room_create: v.room_create,
      room_list: v.room_list,
      room_record: v.room_record,
      hidden: v.hidden,
    });
  });

  const { errors } = form.formState;
  const canPublish = form.watch("can_publish");
  const defaultTtl = project.settings.token_ttl_seconds || 21600;
  const claims = mint.data ? decodeJwtClaims(mint.data.token) : null;

  return (
    <div className="grid items-start gap-4 xl:grid-cols-2">
      <Panel title="Generate an access token" description="For trying things out. In production, your backend mints tokens with your API key, so people cannot give themselves more rights.">
        <form onSubmit={submit} className="flex flex-col gap-3 p-3">
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="Identity" hint="Unique in the room." error={errors.identity?.message}>
              <Input placeholder="alice" {...form.register("identity", { required: "Say who this token is for." })} />
            </Field>
            <Field label="Room" error={errors.room?.message}>
              <Input placeholder="standup" {...form.register("room", { required: "Say which room it joins." })} />
            </Field>
            <Field label="Display name" optional error={errors.name?.message}>
              <Input {...form.register("name")} />
            </Field>
            <Field label="Lifetime in seconds" optional hint={`Default ${defaultTtl}. At most 30 days.`} error={errors.ttl_seconds?.message}>
              <Input type="number" min={1} max={2592000} inputMode="numeric" {...form.register("ttl_seconds")} />
            </Field>
          </div>
          <Field label="Metadata" optional>
            <Textarea rows={2} className="min-h-14 font-mono text-xs" {...form.register("metadata")} />
          </Field>
          <Field label="Attributes" optional hint='A JSON object of strings, like {"role": "host"}.' error={errors.attributes?.message}>
            <Textarea rows={2} className="min-h-14 font-mono text-xs" {...form.register("attributes")} />
          </Field>
          <fieldset>
            <legend className="mb-2 text-sm font-medium">What it can do in the room</legend>
            <div className="grid gap-2 sm:grid-cols-2">
              {GRANTS.map((g) => (
                <Controller
                  key={g.name}
                  control={form.control}
                  name={g.name}
                  render={({ field }) => <CheckRow label={g.label} hint={g.hint} checked={!!field.value} onCheckedChange={field.onChange} />}
                />
              ))}
            </div>
          </fieldset>
          <fieldset disabled={!canPublish} className="disabled:opacity-50">
            <legend className="text-sm font-medium">Sources it may publish</legend>
            <p className="mb-2 text-xs text-ink-3">Tick none to allow every source.</p>
            <Controller
              control={form.control}
              name="sources"
              render={({ field }) => (
                <div className="grid gap-2 sm:grid-cols-2">
                  {TRACK_SOURCES.map((s) => (
                    <CheckRow
                      key={s.value}
                      label={s.label}
                      disabled={!canPublish}
                      checked={field.value.includes(s.value)}
                      onCheckedChange={(v) => field.onChange(v ? [...field.value, s.value] : field.value.filter((x: string) => x !== s.value))}
                    />
                  ))}
                </div>
              )}
            />
          </fieldset>
          <fieldset>
            <legend className="mb-2 text-sm font-medium">What it can do with the API</legend>
            <div className="grid gap-2 sm:grid-cols-2">
              {ADMIN_GRANTS.map((g) => (
                <Controller
                  key={g.name}
                  control={form.control}
                  name={g.name}
                  render={({ field }) => <CheckRow label={g.label} hint={g.hint} checked={!!field.value} onCheckedChange={field.onChange} />}
                />
              ))}
            </div>
          </fieldset>
          {error && (
            <p role="alert" className="text-sm text-critical-text">
              {error}
            </p>
          )}
          {noKey && (
            <Callout tone="serious" title="This project has no API key to sign with">
              <Link className="link" href="/developer">
                Create an API key
              </Link>
              , then generate the token.
            </Callout>
          )}
          <div>
            <Button type="submit" variant="primary" pending={mint.isPending}>
              Generate token
            </Button>
          </div>
        </form>
      </Panel>
      <Panel title="Result" className="xl:sticky xl:top-16">
        {!mint.data ? (
          <p className="p-3 text-sm text-ink-3">Your token will appear here. It is not stored anywhere: generating another replaces it.</p>
        ) : (
          <div className="flex flex-col gap-3 p-3" data-testid="token-result">
            <p className="text-sm text-ink-2">
              Expires <span data-testid="token-expires">{formatDateTime(mint.data.expires_at)}</span>. Signed with this project&rsquo;s newest active API key.
            </p>
            <CodeBlock label="Access token" code={mint.data.token} />
            <CodeBlock label="Connect to" code={mint.data.url} />
            {claims && <CodeBlock label="What it says inside" code={JSON.stringify(claims, null, 2)} />}
          </div>
        )}
      </Panel>
    </div>
  );
}
