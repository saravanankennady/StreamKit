"use client";

import { Command } from "cmdk";
import { CornerDownLeft, Search } from "lucide-react";
import { useRouter } from "next/navigation";
import * as React from "react";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { useProject, useProjectChoice } from "@/lib/session";
import { NAV } from "./nav";

const itemClass = "flex cursor-default items-center gap-2 rounded-xs px-2 py-1.5 text-sm data-[selected=true]:bg-sunken [&_svg]:size-3.5 [&_svg]:text-ink-3";

/** Ctrl+K or Cmd+K: go anywhere, or switch project, without touching the mouse. */
export function CommandPalette() {
  const [open, setOpen] = React.useState(false);
  const router = useRouter();
  const { me, project } = useProject();
  const setProjectId = useProjectChoice((s) => s.setProjectId);

  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key.toLowerCase() === "k" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setOpen((v) => !v);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const go = (href: string) => {
    setOpen(false);
    router.push(href);
  };

  return (
    <>
      <Button variant="secondary" className="hidden w-52 justify-start text-ink-3 md:inline-flex" onClick={() => setOpen(true)}>
        <Search aria-hidden /> Go to
        <kbd className="mono ml-auto rounded-xs border px-1 text-[10px]">Ctrl K</kbd>
      </Button>
      <Button variant="ghost" size="icon" className="md:hidden" aria-label="Go to" onClick={() => setOpen(true)}>
        <Search aria-hidden />
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent title="Go to" className="top-[15%] max-w-xl translate-y-0">
          <Command loop label="Go to" className="flex flex-col">
            <Command.Input placeholder="Page or project name" autoFocus className="h-10 border-b bg-transparent px-4 text-base outline-none placeholder:text-ink-3" />
            <Command.List className="scroll-quiet max-h-80 overflow-y-auto p-2">
              <Command.Empty className="px-2 py-4 text-sm text-ink-3">Nothing matches that.</Command.Empty>
              {NAV.map(({ group, items }) => (
                <Command.Group key={group} heading={group} className="[&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1 [&_[cmdk-group-heading]]:text-xs [&_[cmdk-group-heading]]:text-ink-3">
                  {items.map((item) => (
                    <Command.Item key={item.href} value={item.label} onSelect={() => go(item.href)} className={itemClass}>
                      <item.icon aria-hidden /> {item.label}
                    </Command.Item>
                  ))}
                </Command.Group>
              ))}
              <Command.Group heading="Help and account" className="[&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1 [&_[cmdk-group-heading]]:text-xs [&_[cmdk-group-heading]]:text-ink-3">
                <Command.Item value="Documentation" onSelect={() => go("/docs")} className={itemClass}>
                  Documentation
                </Command.Item>
                <Command.Item value="Account and sessions" onSelect={() => go("/account")} className={itemClass}>
                  Account and sessions
                </Command.Item>
              </Command.Group>
              {me.projects.length > 1 && (
                <Command.Group heading="Switch project" className="[&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1 [&_[cmdk-group-heading]]:text-xs [&_[cmdk-group-heading]]:text-ink-3">
                  {me.projects
                    .filter((p) => p.id !== project.id)
                    .map((p) => (
                      <Command.Item
                        key={p.id}
                        value={`project ${p.name} ${p.id}`}
                        onSelect={() => {
                          setProjectId(p.id);
                          setOpen(false);
                        }}
                        className={itemClass}
                      >
                        <CornerDownLeft aria-hidden /> {p.name}
                      </Command.Item>
                    ))}
                </Command.Group>
              )}
            </Command.List>
          </Command>
        </DialogContent>
      </Dialog>
    </>
  );
}
