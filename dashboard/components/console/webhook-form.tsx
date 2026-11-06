"use client";

import Link from "next/link";
import * as React from "react";
import { Controller, useForm } from "react-hook-form";
import { Callout } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogBody, DialogContent, DialogFooter } from "@/components/ui/dialog";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { CheckRow, Switch } from "@/components/ui/switch";
import { applyApiError } from "@/lib/form";
import { useProject, useProjectApi, useProjectQuery } from "@/lib/session";
import type { ApiKey, Items, Webhook } from "@/lib/types";

interface HookForm {
  url: string;
  api_key_id: string;
  all_events: boolean;
  events: string[];
  enabled: boolean;
}

export function WebhookFormDialog({
  hook,
  open,
  onOpenChange,
  onSaved,
}: {
  hook?: Webhook;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSaved?: (hook: Webhook) => void | Promise<void>;
}) {
  const call = useProjectApi();
  const { canAdmin } = useProject();
  const keys = useProjectQuery<Items<ApiKey>>(["keys"], "/keys", { enabled: open && canAdmin });
  const events = useProjectQuery<Items<string>>(["webhooks", "events"], "/webhooks/events", { enabled: open, staleTime: 600_000 });
  const form = useForm<HookForm>({ defaultValues: { url: "", api_key_id: "", all_events: true, events: [], enabled: true } });
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (!open) return;
    setError(null);
    form.reset(hook ? { url: hook.url, api_key_id: hook.api_key_id, all_events: hook.events.length === 0, events: hook.events, enabled: hook.enabled } : { url: "", api_key_id: "", all_events: true, events: [], enabled: true });
  }, [open, hook, form]);

  // Only keys that can sign: revoked ones fail delivery, but keep the current one listed so its state is visible.
  const usable = (keys.data?.items ?? []).filter((k) => (!k.revoked_at && (!k.expires_at || Date.parse(k.expires_at) > Date.now())) || k.id === hook?.api_key_id);

  React.useEffect(() => {
    if (!open || hook || form.getValues("api_key_id")) return;
    const first = usable[0];
    if (first) form.setValue("api_key_id", first.id);
  }, [open, hook, usable, form]);

  const submit = form.handleSubmit(async (v) => {
    setError(null);
    if (!v.all_events && v.events.length === 0) {
      form.setError("events", { message: "Pick at least one event, or choose all events." });
      return;
    }
    try {
      const body = { url: v.url, api_key_id: v.api_key_id, events: v.all_events ? [] : v.events, enabled: v.enabled };
      const saved = hook ? await call<Webhook>(`/webhooks/${hook.id}`, { method: "PATCH", body }) : await call<Webhook>("/webhooks", { method: "POST", body });
      onOpenChange(false);
      await onSaved?.(saved);
    } catch (err) {
      setError(applyApiError(form, err));
    }
  });

  const { errors, isSubmitting } = form.formState;
  const allEvents = form.watch("all_events");
  const noKeys = keys.data && usable.length === 0;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent title={hook ? "Edit endpoint" : "Add an endpoint"} description="We POST signed JSON to this URL when something happens in this project.">
        <form onSubmit={submit}>
          <DialogBody>
            <Field label="URL" hint="Private and internal addresses are refused unless the server allows them." error={errors.url?.message}>
              <Input autoFocus placeholder="https://example.com/streamkit-events" className="font-mono text-xs" {...form.register("url", { required: "Enter the URL to send events to." })} />
            </Field>
            {noKeys ? (
              <Callout tone="serious" title="Deliveries need an API key to sign with">
                <Link className="link" href="/developer">
                  Create an API key
                </Link>{" "}
                first.
              </Callout>
            ) : (
              <Field label="Signing key" hint="Deliveries are signed with this key's secret. Verify them with the same secret." error={errors.api_key_id?.message}>
                {keys.isPending ? (
                  <Skeleton className="h-8" />
                ) : (
                  <Controller
                    control={form.control}
                    name="api_key_id"
                    rules={{ required: "Choose a key." }}
                    render={({ field }) => (
                      <Select
                        value={field.value}
                        onValueChange={field.onChange}
                        placeholder="Choose a key"
                        options={usable.map((k) => ({ value: k.id, label: `${k.api_key}${k.name ? ` (${k.name})` : ""}` }))}
                      />
                    )}
                  />
                )}
              </Field>
            )}
            <fieldset>
              <legend className="mb-2 text-sm font-medium">Events</legend>
              <Controller control={form.control} name="all_events" render={({ field }) => <CheckRow label="All events" hint="Including any added later." checked={field.value} onCheckedChange={field.onChange} />} />
              {!allEvents && (
                <Controller
                  control={form.control}
                  name="events"
                  render={({ field }) => (
                    <div className="mt-2 grid gap-1.5 border-t pt-2 sm:grid-cols-2">
                      {(events.data?.items ?? []).map((name) => (
                        <CheckRow
                          key={name}
                          label={<span className="mono text-xs">{name}</span>}
                          checked={field.value.includes(name)}
                          onCheckedChange={(v) => field.onChange(v ? [...field.value, name] : field.value.filter((x) => x !== name))}
                        />
                      ))}
                    </div>
                  )}
                />
              )}
              {errors.events?.message && (
                <p role="alert" className="mt-1 text-xs text-critical-text">
                  {errors.events.message}
                </p>
              )}
            </fieldset>
            <label className="flex items-center gap-2 text-sm">
              <Controller control={form.control} name="enabled" render={({ field }) => <Switch checked={field.value} onCheckedChange={field.onChange} aria-label="Enabled" />} />
              Send events to this endpoint
            </label>
            {error && (
              <p role="alert" className="text-sm text-critical-text">
                {error}
              </p>
            )}
          </DialogBody>
          <DialogFooter>
            <Button onClick={() => onOpenChange(false)}>Cancel</Button>
            <Button type="submit" variant="primary" pending={isSubmitting} disabled={!!noKeys}>
              {hook ? "Save endpoint" : "Add endpoint"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
