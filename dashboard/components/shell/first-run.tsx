"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ArrowRight, TriangleAlert } from "lucide-react";
import * as React from "react";
import { Controller, useForm } from "react-hook-form";
import { Button } from "@/components/ui/button";
import { Callout } from "@/components/ui/badge";
import { CodeBlock, CopyValue } from "@/components/ui/copy";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { toastError } from "@/components/ui/toast";
import { api, ApiError, httpOrigin } from "@/lib/api";
import { applyApiError } from "@/lib/form";
import { meKey, useProjectChoice, useSignOut } from "@/lib/session";
import type { ApiKey, Environment, Me, Project } from "@/lib/types";
import { Wordmark } from "./mark";
import { ThemeMenu } from "./theme-menu";

export const ENVIRONMENTS = [
  { value: "development", label: "Development" },
  { value: "staging", label: "Staging" },
  { value: "production", label: "Production" },
];

interface ProjectForm {
  name: string;
  region: string;
  environment: Environment;
}

function Step({ n, title, done, children }: { n: number; title: string; done?: boolean; children?: React.ReactNode }) {
  return (
    <li className="grid grid-cols-[1.75rem_minmax(0,1fr)] gap-x-3 border-t py-5">
      <span aria-hidden className={`num flex size-7 items-center justify-center rounded-xs border text-sm ${done ? "border-ink bg-ink text-on-ink" : "border-line-2"}`}>
        {n}
      </span>
      <div className="min-w-0">
        <h2 className="pt-0.5 text-lg font-semibold">{title}</h2>
        {children && <div className="mt-3">{children}</div>}
      </div>
    </li>
  );
}

/** What someone sees before they have a project: create one, get a key, and see how to use them. */
export function FirstRun({ me }: { me: Me }) {
  const qc = useQueryClient();
  const setProjectId = useProjectChoice((s) => s.setProjectId);
  const signOut = useSignOut();
  const [project, setProject] = React.useState<Project | null>(null);
  const [key, setKey] = React.useState<ApiKey | null>(null);
  const form = useForm<ProjectForm>({ defaultValues: { name: "", region: "", environment: "development" } });
  const [formError, setFormError] = React.useState<string | null>(null);

  const create = useMutation<Project, ApiError, ProjectForm>({
    mutationFn: (v) => api<Project>("/projects", { method: "POST", body: { name: v.name, region: v.region || undefined, environment: v.environment } }),
    onSuccess: (p) => {
      setProject(p);
      setFormError(null);
    },
    onError: (err) => setFormError(applyApiError(form, err)),
  });

  const createKey = useMutation<ApiKey, ApiError>({
    mutationFn: () => api<ApiKey>("/keys", { method: "POST", project: project?.id, body: { name: "first key" } }),
    onSuccess: setKey,
    onError: (err) => toastError(err, "Could not create the key"),
  });

  const finish = () => {
    if (project) setProjectId(project.id);
    void qc.invalidateQueries({ queryKey: meKey });
  };

  return (
    <div className="min-h-dvh bg-page">
      <header className="flex h-12 items-center justify-between border-b bg-surface px-4">
        <Wordmark />
        <div className="flex items-center gap-2">
          <span className="hidden text-sm text-ink-3 sm:inline">{me.user.email}</span>
          <ThemeMenu />
          <Button variant="ghost" size="sm" onClick={signOut}>
            Sign out
          </Button>
        </div>
      </header>
      <main className="mx-auto max-w-2xl px-4 py-8 md:py-12">
        <p className="text-sm text-ink-3">Welcome, {me.user.name}</p>
        <h1 className="mt-1 mb-6 text-2xl font-semibold tracking-tight">Set up your first project</h1>
        <ol className="border-b">
          <Step n={1} title={project ? `Project “${project.name}” created` : "Create a project"} done={!!project}>
            {!project ? (
              <form
                className="flex max-w-md flex-col gap-3"
                onSubmit={form.handleSubmit((v) => create.mutate(v))}
              >
                <p className="text-sm text-ink-2">A project holds rooms, API keys and webhooks. Nothing is shared between projects. Make one per app and environment.</p>
                <Field label="Name" error={form.formState.errors.name?.message}>
                  <Input autoFocus placeholder="my-app" {...form.register("name", { required: "Give the project a name." })} />
                </Field>
                <Field label="Environment">
                  <Controller control={form.control} name="environment" render={({ field }) => <Select value={field.value} onValueChange={field.onChange} options={ENVIRONMENTS} />} />
                </Field>
                <Field label="Region" optional hint="A label for your own reference. It does not change where media is routed yet." error={form.formState.errors.region?.message}>
                  <Input placeholder="default" {...form.register("region")} />
                </Field>
                {formError && (
                  <p role="alert" className="text-sm text-critical-text">
                    {formError}
                  </p>
                )}
                <div>
                  <Button type="submit" variant="primary" pending={create.isPending}>
                    Create project
                  </Button>
                </div>
              </form>
            ) : (
              <dl className="grid gap-1 text-sm">
                <div className="flex flex-wrap gap-2">
                  <dt className="text-ink-3">Clients connect to</dt>
                  <dd>
                    <CopyValue value={project.url} />
                  </dd>
                </div>
              </dl>
            )}
          </Step>
          <Step n={2} title={key ? "API key created" : "Create an API key"} done={!!key}>
            {project && !key && (
              <div className="flex flex-col items-start gap-3">
                <p className="text-sm text-ink-2">Your backend signs access tokens with this key. You can also skip this and create one later in Developer settings.</p>
                <div className="flex flex-wrap gap-2">
                  <Button variant="primary" pending={createKey.isPending} onClick={() => createKey.mutate()}>
                    Create API key
                  </Button>
                  <Button variant="ghost" onClick={finish}>
                    Skip for now
                  </Button>
                </div>
              </div>
            )}
            {key && (
              <div className="flex flex-col gap-2">
                <Callout tone="warn" title="Copy the secret now. It will not be shown again." />
                <div className="grid gap-1 text-sm">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="w-16 text-ink-3">API key</span>
                    <CopyValue value={key.api_key} />
                  </div>
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="w-16 text-ink-3">Secret</span>
                    <CopyValue value={key.secret ?? ""} />
                  </div>
                </div>
              </div>
            )}
          </Step>
          <Step n={3} title="Open a room">
            {project && key && (
              <div className="flex flex-col items-start gap-3">
                <p className="text-sm text-ink-2">Rooms open by themselves when the first person joins with a valid token, or you can create one from the API. Once you are in, the Rooms page can mint a token for this command.</p>
                <CodeBlock
                  className="w-full"
                  label="Create a room"
                  code={`curl -X POST ${httpOrigin(project.url)}/api/v1/rooms \\
  -H "Authorization: Bearer $STREAMKIT_TOKEN" \\
  -H "Content-Type: application/json" \\
  -d '{"name": "my-room"}'`}
                />
                <Button variant="primary" onClick={finish}>
                  Go to the console <ArrowRight aria-hidden />
                </Button>
              </div>
            )}
          </Step>
        </ol>
        {!me.organizations.length && (
          <p className="mt-4 flex items-center gap-2 text-sm text-warn-text">
            <TriangleAlert className="size-4" aria-hidden /> You are not in an organization, so you cannot create a project. Ask an owner to add you.
          </p>
        )}
      </main>
    </div>
  );
}
