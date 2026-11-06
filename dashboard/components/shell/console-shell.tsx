"use client";

import * as DialogPrimitive from "@radix-ui/react-dialog";
import { useMutation } from "@tanstack/react-query";
import { BookOpen, LogOut, Menu, ShieldCheck, UserRound } from "lucide-react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import * as React from "react";
import { Callout } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { SheetContent } from "@/components/ui/dialog";
import { Dropdown, DropdownContent, DropdownItem, DropdownLabel, DropdownSeparator, DropdownTrigger } from "@/components/ui/dropdown";
import { Skeleton } from "@/components/ui/skeleton";
import { toast, toastError } from "@/components/ui/toast";
import { api, ApiError } from "@/lib/api";
import { PRODUCT_NAME } from "@/lib/brand";
import { useLive } from "@/lib/live";
import { ProjectContext, useMe, useProject, useProjectChoice, useSignOut, type ProjectContextValue } from "@/lib/session";
import { CommandPalette } from "./command-palette";
import { FirstRun } from "./first-run";
import { Wordmark } from "./mark";
import { NAV } from "./nav";
import { ProjectSwitcher } from "./project-switcher";
import { SideNav, Sidebar } from "./sidebar";
import { StatusLine } from "./status-line";
import { ThemeMenu } from "./theme-menu";

function UserMenu() {
  const { me } = useProject();
  const router = useRouter();
  const signOut = useSignOut();
  return (
    <Dropdown>
      <DropdownTrigger asChild>
        <Button variant="ghost" size="icon" aria-label="Account menu" data-testid="user-menu">
          <UserRound aria-hidden />
        </Button>
      </DropdownTrigger>
      <DropdownContent className="w-60">
        <DropdownLabel>
          <span className="block truncate font-medium text-ink">{me.user.name}</span>
          <span className="block truncate">{me.user.email}</span>
        </DropdownLabel>
        <DropdownSeparator />
        <DropdownItem onSelect={() => router.push("/account")}>
          <ShieldCheck aria-hidden /> Account and sessions
        </DropdownItem>
        <DropdownItem onSelect={() => router.push("/docs")}>
          <BookOpen aria-hidden /> Documentation
        </DropdownItem>
        <DropdownSeparator />
        <DropdownItem onSelect={signOut}>
          <LogOut aria-hidden /> Sign out
        </DropdownItem>
      </DropdownContent>
    </Dropdown>
  );
}

function VerifyEmailBanner() {
  const { me } = useProject();
  const resend = useMutation<unknown, ApiError>({
    mutationFn: () => api("/auth/resend-verification", { method: "POST" }),
    onSuccess: () => toast("Confirmation email sent", `Check ${me.user.email}.`),
    onError: (err) => toastError(err, "Could not send the email"),
  });
  if (me.user.email_verified_at) return null;
  return (
    <div className="border-b px-4 py-2 md:px-6">
      <Callout
        tone="warn"
        title={`Confirm ${me.user.email}`}
        action={
          <Button size="sm" pending={resend.isPending} onClick={() => resend.mutate()}>
            Send the email again
          </Button>
        }
      >
        You will need this address to reset your password. Check your inbox for the confirmation link.
      </Callout>
    </div>
  );
}

function ProjectBanner() {
  const { project } = useProject();
  if (project.status !== "suspended") return null;
  return (
    <div className="border-b px-4 py-2 md:px-6">
      <Callout tone="serious" title={`${project.name} is suspended`} action={<Button size="sm" asChild><Link href={`/projects/${project.id}`}>Open project</Link></Button>}>
        Its keys do not verify, so nobody can connect. Rooms already open keep running until they close.
      </Callout>
    </div>
  );
}

function TopBar() {
  const [navOpen, setNavOpen] = React.useState(false);
  return (
    <header className="sticky top-0 z-30 flex h-12 items-center gap-2 border-b bg-surface px-3 md:px-4">
      <DialogPrimitive.Root open={navOpen} onOpenChange={setNavOpen}>
        <DialogPrimitive.Trigger asChild>
          <Button variant="ghost" size="icon" className="lg:hidden" aria-label="Open navigation">
            <Menu aria-hidden />
          </Button>
        </DialogPrimitive.Trigger>
        <SheetContent title="Navigation">
          <div className="flex h-12 shrink-0 items-center border-b px-3">
            <Wordmark />
          </div>
          <SideNav onNavigate={() => setNavOpen(false)} />
        </SheetContent>
      </DialogPrimitive.Root>
      <ProjectSwitcher />
      <div className="ml-auto flex items-center gap-1">
        <CommandPalette />
        <ThemeMenu />
        <UserMenu />
      </div>
    </header>
  );
}

function usePageTitle() {
  const pathname = usePathname();
  React.useEffect(() => {
    const item = NAV.flatMap((g) => g.items).find((i) => pathname === i.href || pathname.startsWith(`${i.href}/`));
    const label = item?.label ?? (pathname.startsWith("/account") ? "Account and sessions" : null);
    document.title = label ? `${label} · ${PRODUCT_NAME}` : PRODUCT_NAME;
  }, [pathname]);
}

function ConsoleFrame({ children }: { children: React.ReactNode }) {
  usePageTitle();
  return (
    <div className="flex min-h-dvh flex-col">
      <a href="#content" className="sr-only rounded-xs bg-ink px-3 py-2 text-on-ink focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-50">
        Skip to content
      </a>
      <div className="flex min-h-0 flex-1">
        <Sidebar />
        <div className="flex min-w-0 flex-1 flex-col">
          <TopBar />
          <VerifyEmailBanner />
          <ProjectBanner />
          <main id="content" className="min-w-0 flex-1">
            {children}
          </main>
        </div>
      </div>
      <StatusLine />
    </div>
  );
}

function LoadingFrame() {
  return (
    <div role="status" aria-label="Loading the console" className="flex min-h-dvh">
      <div className="hidden w-52 border-r bg-surface lg:block" />
      <div className="flex flex-1 flex-col gap-4 p-6">
        <Skeleton className="h-6 w-48" />
        <Skeleton className="h-4 w-80 max-w-full" />
        <Skeleton className="h-40 w-full" />
      </div>
    </div>
  );
}

function ConnectionProblem({ error, onRetry }: { error: ApiError; onRetry: () => void }) {
  return (
    <main className="flex min-h-dvh flex-col items-start justify-center gap-3 px-6 md:px-16">
      <Wordmark />
      <h1 className="text-2xl font-semibold tracking-tight">{error.unreachable ? "The server is not responding" : "The console could not load"}</h1>
      <p className="max-w-lg text-ink-2">{error.unreachable && error.status !== 0 ? "Check that streamkit-server is running, and that this dashboard can reach it. Then try again." : error.message}</p>
      <Button variant="primary" onClick={onRetry}>
        Try again
      </Button>
    </main>
  );
}

/** Everything behind sign-in: the user, the project, the live feed. */
export function ConsoleShell({ children, allowNoProject }: { children: React.ReactNode; allowNoProject?: boolean }) {
  const me = useMe();
  const projectId = useProjectChoice((s) => s.projectId);
  const start = useLive((s) => s.start);

  const project = me.data?.projects.find((p) => p.id === projectId) ?? me.data?.projects[0] ?? null;
  const currentId = project?.id ?? null;

  React.useEffect(() => {
    if (currentId) start(currentId);
    else useLive.getState().stop();
  }, [currentId, start]);

  const value = React.useMemo<ProjectContextValue | null>(() => {
    if (!me.data || !project) return null;
    const role = me.data.organizations.find((o) => o.id === project.org_id)?.role ?? "member";
    return { me: me.data, project, projectId: project.id, role, canAdmin: role !== "member" };
  }, [me.data, project]);

  if (me.isPending) return <LoadingFrame />;
  // A 401 is on its way to the sign-in page, from the query cache's error handler.
  if (me.error) return me.error.status === 401 ? <LoadingFrame /> : <ConnectionProblem error={me.error} onRetry={() => me.refetch()} />;
  if (!value) return allowNoProject ? <>{children}</> : <FirstRun me={me.data} />;

  return (
    <ProjectContext.Provider value={value}>
      <ConsoleFrame>{children}</ConsoleFrame>
    </ProjectContext.Provider>
  );
}
