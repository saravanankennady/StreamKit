"use client";

import { useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import * as React from "react";
import { Controller, useForm } from "react-hook-form";
import { Page, Panel } from "@/components/console/page";
import { ENVIRONMENTS } from "@/components/shell/first-run";
import { StatusBadge, Tag } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CopyValue } from "@/components/ui/copy";
import { Dialog, DialogBody, DialogContent, DialogFooter } from "@/components/ui/dialog";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { toast } from "@/components/ui/toast";
import { api } from "@/lib/api";
import { formatShortTime } from "@/lib/format";
import { applyApiError } from "@/lib/form";
import { meKey, useProject, useProjectChoice } from "@/lib/session";
import type { Environment, Project } from "@/lib/types";

function CreateProjectDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const { me } = useProject();
  const qc = useQueryClient();
  const setProjectId = useProjectChoice((s) => s.setProjectId);
  const form = useForm<{ name: string; region: string; environment: Environment; org_id: string }>({
    defaultValues: { name: "", region: "", environment: "development", org_id: me.organizations[0]?.id ?? "" },
  });
  const [error, setError] = React.useState<string | null>(null);
  React.useEffect(() => {
    if (open) {
      form.reset();
      setError(null);
    }
  }, [open, form]);
  const adminOrgs = me.organizations.filter((o) => o.role !== "member");
  const submit = form.handleSubmit(async (v) => {
    setError(null);
    try {
      const p = await api<Project>("/projects", { method: "POST", body: { name: v.name, region: v.region || undefined, environment: v.environment, org_id: v.org_id || undefined } });
      await qc.invalidateQueries({ queryKey: meKey });
      setProjectId(p.id);
      onOpenChange(false);
      toast(`Switched to ${p.name}`, "It has no API keys yet. Create one in Developer settings.");
    } catch (err) {
      setError(applyApiError(form, err));
    }
  });
  const { errors, isSubmitting } = form.formState;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent title="Create a project" description="Rooms, API keys and webhooks are not shared between projects.">
        <form onSubmit={submit}>
          <DialogBody>
            <Field label="Name" error={errors.name?.message}>
              <Input autoFocus {...form.register("name", { required: "Give the project a name." })} />
            </Field>
            {me.organizations.length > 1 && (
              <Field label="Organization">
                <Controller
                  control={form.control}
                  name="org_id"
                  render={({ field }) => <Select value={field.value} onValueChange={field.onChange} options={adminOrgs.map((o) => ({ value: o.id, label: o.name }))} />}
                />
              </Field>
            )}
            <Field label="Environment">
              <Controller control={form.control} name="environment" render={({ field }) => <Select value={field.value} onValueChange={field.onChange} options={ENVIRONMENTS} />} />
            </Field>
            <Field label="Region" optional hint="A label for your own reference. It does not change where media is routed yet." error={errors.region?.message}>
              <Input placeholder="default" {...form.register("region")} />
            </Field>
            {error && (
              <p role="alert" className="text-sm text-critical-text">
                {error}
              </p>
            )}
          </DialogBody>
          <DialogFooter>
            <Button onClick={() => onOpenChange(false)}>Cancel</Button>
            <Button type="submit" variant="primary" pending={isSubmitting}>
              Create project
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export default function ProjectsPage() {
  const { me, project: current } = useProject();
  const setProjectId = useProjectChoice((s) => s.setProjectId);
  const params = useSearchParams();
  const router = useRouter();
  const [creating, setCreating] = React.useState(params.get("new") === "1");
  return (
    <Page
      title="Projects"
      description="A project is an isolated space with its own rooms, keys and webhooks. Make one per app and environment."
      actions={
        <Button
          variant="primary"
          onClick={() => setCreating(true)}
          disabled={!me.organizations.some((o) => o.role !== "member")}
        >
          <Plus aria-hidden /> New project
        </Button>
      }
    >
      <Panel>
        <Table>
          <thead>
            <tr>
              <Th>Name</Th>
              <Th className="hidden lg:table-cell">ID</Th>
              <Th className="hidden md:table-cell">Connect to</Th>
              <Th className="hidden sm:table-cell">Region</Th>
              <Th className="hidden sm:table-cell">Environment</Th>
              <Th>Status</Th>
              <Th className="hidden xl:table-cell">Created</Th>
              <Th className="w-20">
                <span className="sr-only">Switch</span>
              </Th>
            </tr>
          </thead>
          <tbody>
            {me.projects.map((p) => (
              <Tr key={p.id} data-testid="project-row" data-name={p.name}>
                <Td className="max-w-52">
                  <Link href={`/projects/${p.id}`} className="link block truncate font-medium">
                    {p.name}
                  </Link>
                </Td>
                <Td className="hidden lg:table-cell">
                  <CopyValue value={p.id} display={`${p.id.slice(0, 8)}…`} className="text-xs text-ink-2" />
                </Td>
                <Td className="hidden md:table-cell">
                  <CopyValue value={p.url} className="text-xs text-ink-2" />
                </Td>
                <Td className="hidden text-ink-2 sm:table-cell">{p.region}</Td>
                <Td className="hidden sm:table-cell">
                  <Tag>{p.environment}</Tag>
                </Td>
                <Td>{p.status === "active" ? <StatusBadge tone="good">Active</StatusBadge> : <StatusBadge tone="serious">Suspended</StatusBadge>}</Td>
                <Td className="hidden whitespace-nowrap text-ink-2 xl:table-cell">{formatShortTime(p.created_at)}</Td>
                <Td className="text-right whitespace-nowrap">
                  {p.id === current.id ? (
                    <span className="text-xs text-ink-3">Current</span>
                  ) : (
                    <Button
                      size="sm"
                      onClick={() => {
                        setProjectId(p.id);
                        router.push("/overview");
                      }}
                    >
                      Switch
                    </Button>
                  )}
                </Td>
              </Tr>
            ))}
          </tbody>
        </Table>
      </Panel>
      <CreateProjectDialog open={creating} onOpenChange={setCreating} />
    </Page>
  );
}
