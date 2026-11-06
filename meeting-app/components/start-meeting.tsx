"use client";

import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useState } from "react";
import { Button } from "./ui";

interface Project {
  id: string;
  name: string;
}

const JOIN_MARK = "/meet/join#";
const PROJECT_KEY = "streamkit.project";

/** The dashboard remembers which project someone is working in, on this same address. Start from that one. */
function rememberedProject(): string | null {
  try {
    return JSON.parse(localStorage.getItem(PROJECT_KEY) ?? "{}")?.state?.projectId ?? null;
  } catch {
    return null;
  }
}

type Signed = { state: "checking" } | { state: "out" } | { state: "in"; projects: Project[] } | { state: "unreachable" };

// Unlike joining, this uses the person's dashboard session on purpose: starting a meeting takes an account, because it
// makes a host link on a project, and the server refuses anyone without one.
async function get<T>(path: string): Promise<{ status: number; data: T | null }> {
  const res = await fetch(`/api/v1${path}`, { credentials: "same-origin" });
  return { status: res.status, data: (await res.json().catch(() => null)) as T | null };
}

export function StartMeeting() {
  const router = useRouter();
  const [signed, setSigned] = useState<Signed>({ state: "checking" });
  const [title, setTitle] = useState("");
  const [project, setProject] = useState("");
  const [waiting, setWaiting] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ text: string; needsKey?: boolean } | null>(null);

  useEffect(() => {
    let live = true;
    get<{ items?: Project[] } | Project[]>("/projects")
      .then(({ status, data }) => {
        if (!live) return;
        if (status === 401) {
          setSigned({ state: "out" });
          return;
        }
        if (status !== 200) {
          setSigned({ state: "unreachable" });
          return;
        }
        const projects = (Array.isArray(data) ? data : data?.items) ?? [];
        setSigned({ state: "in", projects });
        const remembered = rememberedProject();
        setProject(projects.find((p) => p.id === remembered)?.id ?? projects[0]?.id ?? "");
      })
      .catch(() => {
        if (live) setSigned({ state: "unreachable" });
      });
    return () => {
      live = false;
    };
  }, []);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const res = await fetch("/api/v1/meetings", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json", "X-Project-ID": project },
        body: JSON.stringify({ title: title.trim(), waiting_room: waiting }),
      });
      const data = await res.json().catch(() => null);
      if (!res.ok) {
        setError({ text: data?.error?.message ?? "Could not start the meeting. Try again.", needsKey: data?.error?.code === "no_api_key" });
        setBusy(false);
        return;
      }
      // The host link goes in the fragment like any other, so it stays out of every log on the way. The join page takes it
      // out of the address once it has read it.
      const url: string = data.host_url;
      const code = url.slice(url.indexOf(JOIN_MARK) + JOIN_MARK.length);
      router.push(`/join#${code}`);
    } catch {
      setError({ text: "Could not reach the server. Check your connection and try again." });
      setBusy(false);
    }
  };

  return (
    <section aria-labelledby="start-heading" data-testid="start-meeting">
      <h1 id="start-heading" className="text-xl font-semibold">
        Start a meeting
      </h1>
      {signed.state === "checking" && (
        <p role="status" className="mt-2 text-sm text-ink-2">
          Checking whether you are signed in&hellip;
        </p>
      )}
      {signed.state === "unreachable" && (
        <p role="alert" className="mt-2 text-sm text-critical-text">
          Could not reach the server to check.
        </p>
      )}
      {signed.state === "out" && (
        <div data-testid="start-signed-out">
          <p className="mt-2 text-sm text-ink-2">Starting a meeting takes an account, so that a stranger cannot run rooms on this server. Joining one does not.</p>
          <a href="/login?next=/meet" data-testid="start-sign-in" className="mt-3 inline-flex min-h-10 items-center justify-center rounded-md bg-raised px-4 text-sm font-medium hover:bg-line-2">
            Sign in
          </a>
        </div>
      )}
      {signed.state === "in" && signed.projects.length === 0 && (
        <p className="mt-2 text-sm text-ink-2" data-testid="start-no-project">
          You do not have a project yet. <a className="underline" href="/projects">Make one in the dashboard</a>, then come back.
        </p>
      )}
      {signed.state === "in" && signed.projects.length > 0 && (
        <form onSubmit={submit} className="mt-4 flex flex-col gap-3">
          <label className="flex flex-col gap-1 text-sm font-medium">
            Title
            <input data-testid="start-title" value={title} maxLength={100} onChange={(e) => setTitle(e.target.value)} placeholder="Weekly catch-up" className="h-10 rounded-md border border-line-2 bg-surface px-3 font-normal" />
          </label>
          {signed.projects.length > 1 && (
            <label className="flex flex-col gap-1 text-sm font-medium">
              Project
              <select data-testid="start-project" value={project} onChange={(e) => setProject(e.target.value)} className="h-10 rounded-md border border-line-2 bg-surface px-2 font-normal">
                {signed.projects.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
            </label>
          )}
          <label className="flex items-center gap-2 text-sm">
            <input data-testid="start-waiting" type="checkbox" checked={waiting} onChange={(e) => setWaiting(e.target.checked)} className="size-4" />
            People wait until I let them in
          </label>
          {error && (
            <p role="alert" data-testid="start-error" className="text-sm text-critical-text">
              {error.text}{" "}
              {error.needsKey && (
                <a className="underline" href="/developer">
                  Add a key in the dashboard
                </a>
              )}
            </p>
          )}
          <Button type="submit" variant="primary" disabled={busy || !project} data-testid="start-button">
            {busy ? "Starting…" : "Start meeting"}
          </Button>
        </form>
      )}
    </section>
  );
}
