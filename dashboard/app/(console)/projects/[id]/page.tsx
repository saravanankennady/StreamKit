"use client";

import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeft } from "lucide-react";
import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import * as React from "react";
import { Controller, useForm } from "react-hook-form";
import { EmptyState, Facts, Page, Panel } from "@/components/console/page";
import { ENVIRONMENTS } from "@/components/shell/first-run";
import { Callout, StatusBadge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { CopyValue } from "@/components/ui/copy";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { toast } from "@/components/ui/toast";
import { api } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { applyApiError } from "@/lib/form";
import { meKey, useProject, useProjectChoice } from "@/lib/session";
import type { Environment, Project } from "@/lib/types";

export default function ProjectPage() {
  const { id } = useParams<{ id: string }>();
  const { me, project: current } = useProject();
  const project = me.projects.find((p) => p.id === id);
  const role = me.organizations.find((o) => o.id === project?.org_id)?.role ?? "member";
  const canAdmin = role !== "member";
  const qc = useQueryClient();
  const router = useRouter();
  const setProjectId = useProjectChoice((s) => s.setProjectId);
  const [dialog, setDialog] = React.useState<"delete" | "suspend" | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const form = useForm<{ name: string; region: string; environment: Environment }>({
    values: project ? { name: project.name, region: project.region, environment: project.environment } : undefined,
  });

  if (!project) {
    return (
      <Page title="Project not found">
        <Panel>
          <EmptyState
            title="You cannot see this project"
            action={
              <Button asChild>
                <Link href="/projects">All projects</Link>
              </Button>
            }
          >
            <p>It may have been deleted, or it belongs to an organization you are not in.</p>
          </EmptyState>
        </Panel>
      </Page>
    );
  }

  const submit = form.handleSubmit(async (v) => {
    setError(null);
    try {
      await api<Project>(`/projects/${project.id}`, { method: "PATCH", body: v });
      await qc.invalidateQueries({ queryKey: meKey });
      toast("Project saved");
    } catch (err) {
      setError(applyApiError(form, err));
    }
  });

  const setStatus = async (status: "active" | "suspended") => {
    await api<Project>(`/projects/${project.id}`, { method: "PATCH", body: { status } });
    await qc.invalidateQueries({ queryKey: meKey });
    toast(status === "active" ? "Project reactivated" : "Project suspended");
  };

  const { errors, isSubmitting, isDirty } = form.formState;
  return (
    <Page
      title={project.name}
      description={
        <Link href="/projects" className="link inline-flex items-center gap-1 text-sm text-ink-2">
          <ArrowLeft className="size-3" aria-hidden /> All projects
        </Link>
      }
      actions={
        project.id !== current.id && (
          <Button onClick={() => setProjectId(project.id)}>Switch to this project</Button>
        )
      }
    >
      {!canAdmin && <Callout title="You can look, but not change this project">Your role in this organization is member. Owners and admins can edit it.</Callout>}
      <Panel title="Details">
        <Facts
          items={[
            { label: "Project ID", value: <CopyValue value={project.id} />, wide: true },
            { label: "Clients connect to", value: <CopyValue value={project.url} /> },
            { label: "Status", value: project.status === "active" ? <StatusBadge tone="good">Active</StatusBadge> : <StatusBadge tone="serious">Suspended</StatusBadge> },
            { label: "Created", value: formatDateTime(project.created_at) },
            { label: "Last changed", value: formatDateTime(project.updated_at) },
          ]}
        />
      </Panel>
      <Panel title="Name, environment and region">
        <form onSubmit={submit} className="flex max-w-md flex-col gap-3 p-3">
          <fieldset disabled={!canAdmin} className="contents">
            <Field label="Name" error={errors.name?.message}>
              <Input {...form.register("name", { required: "A project needs a name." })} />
            </Field>
            <Field label="Environment" error={errors.environment?.message}>
              <Controller control={form.control} name="environment" render={({ field }) => <Select value={field.value} onValueChange={field.onChange} options={ENVIRONMENTS} disabled={!canAdmin} />} />
            </Field>
            <Field label="Region" hint="A label for your own reference. It does not change where media is routed yet." error={errors.region?.message}>
              <Input {...form.register("region", { required: "Region cannot be empty once set. Use default if you have no preference." })} />
            </Field>
            {error && (
              <p role="alert" className="text-sm text-critical-text">
                {error}
              </p>
            )}
            <div>
              <Button type="submit" variant="primary" pending={isSubmitting} disabled={!isDirty}>
                Save changes
              </Button>
            </div>
          </fieldset>
        </form>
      </Panel>
      <Panel title="Settings">
        <p className="p-3 text-sm text-ink-2">
          Allowed origins, room defaults, token lifetime and TURN servers are in{" "}
          {project.id === current.id ? (
            <Link className="link" href="/developer?tab=settings">
              Developer settings
            </Link>
          ) : (
            "Developer settings, once you switch to this project"
          )}
          .
        </p>
      </Panel>
      {canAdmin && (
        <Panel title="Danger zone">
          <div className="flex flex-wrap items-center justify-between gap-3 border-b p-3">
            <div className="max-w-xl">
              <p className="font-medium">{project.status === "active" ? "Suspend this project" : "Reactivate this project"}</p>
              <p className="text-sm text-ink-2">{project.status === "active" ? "Its API keys stop verifying at once, so nobody new can connect. Reversible." : "Its API keys start working again."}</p>
            </div>
            {project.status === "active" ? (
              <Button variant="danger" onClick={() => setDialog("suspend")}>
                Suspend project
              </Button>
            ) : (
              <Button onClick={() => setStatus("active")}>Reactivate project</Button>
            )}
          </div>
          <div className="flex flex-wrap items-center justify-between gap-3 p-3">
            <div className="max-w-xl">
              <p className="font-medium">Delete this project</p>
              <p className="text-sm text-ink-2">Revokes every API key and closes every open room. This cannot be undone from here.</p>
            </div>
            <Button variant="danger" onClick={() => setDialog("delete")}>
              Delete project
            </Button>
          </div>
        </Panel>
      )}
      <ConfirmDialog
        open={dialog === "suspend"}
        onOpenChange={(o) => !o && setDialog(null)}
        title={`Suspend ${project.name}?`}
        description="Its API keys stop verifying at once, so nobody new can connect. Rooms already open keep running until they close. You can reactivate it later."
        confirmLabel="Suspend project"
        onConfirm={() => setStatus("suspended")}
      />
      <ConfirmDialog
        open={dialog === "delete"}
        onOpenChange={(o) => !o && setDialog(null)}
        title={`Delete ${project.name}?`}
        description="This revokes all of its API keys and closes all of its rooms, disconnecting everyone in them."
        confirmLabel="Delete project"
        typeToConfirm={project.name}
        onConfirm={async () => {
          await api(`/projects/${project.id}`, { method: "DELETE" });
          setProjectId(null);
          await qc.invalidateQueries({ queryKey: meKey });
          toast(`${project.name} deleted`);
          router.push("/projects");
        }}
      />
    </Page>
  );
}
