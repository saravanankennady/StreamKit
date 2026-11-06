"use client";

import { Check, ChevronsUpDown, Plus, Settings2 } from "lucide-react";
import { useRouter } from "next/navigation";
import { Tag } from "@/components/ui/badge";
import { Dropdown, DropdownContent, DropdownItem, DropdownLabel, DropdownSeparator, DropdownTrigger } from "@/components/ui/dropdown";
import { useProject, useProjectChoice } from "@/lib/session";

export function ProjectSwitcher() {
  const { me, project } = useProject();
  const setProjectId = useProjectChoice((s) => s.setProjectId);
  const router = useRouter();
  return (
    <Dropdown>
      <DropdownTrigger
        data-testid="project-switcher"
        className="flex h-8 max-w-[13rem] min-w-0 items-center gap-2 rounded-sm border border-line-2 bg-surface px-2 text-sm hover:bg-sunken sm:max-w-xs"
      >
        <span className="truncate font-medium">{project.name}</span>
        <Tag className="hidden md:inline-flex">{project.environment}</Tag>
        <ChevronsUpDown className="ml-auto size-3.5 shrink-0 text-ink-3" aria-hidden />
      </DropdownTrigger>
      <DropdownContent align="start" className="w-72">
        <DropdownLabel>Switch project</DropdownLabel>
        {me.projects.map((p) => (
          <DropdownItem key={p.id} onSelect={() => setProjectId(p.id)}>
            <span className="min-w-0 flex-1 truncate">{p.name}</span>
            <Tag>{p.environment}</Tag>
            {p.id === project.id ? <Check className="!text-ink" strokeWidth={3} aria-label="Current project" /> : <span className="size-3.5" />}
          </DropdownItem>
        ))}
        <DropdownSeparator />
        <DropdownItem onSelect={() => router.push("/projects")}>
          <Settings2 aria-hidden /> Manage projects
        </DropdownItem>
        <DropdownItem onSelect={() => router.push("/projects?new=1")}>
          <Plus aria-hidden /> New project
        </DropdownItem>
      </DropdownContent>
    </Dropdown>
  );
}
