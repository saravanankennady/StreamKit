"use client";

import { useMutation, useQuery, useQueryClient, type UseQueryOptions } from "@tanstack/react-query";
import * as React from "react";
import { create } from "zustand";
import { persist } from "zustand/middleware";
import { api, ApiError, type RequestOptions } from "./api";
import { useLive } from "./live";
import type { Me, Project, ProjectSettings } from "./types";

// The signed-in user and the project they are looking at. Project-scoped query keys start with the project ID, so
// switching projects can never show another project's cached data.

interface ProjectChoice {
  projectId: string | null;
  setProjectId: (id: string | null) => void;
}

export const useProjectChoice = create<ProjectChoice>()(
  persist((set) => ({ projectId: null, setProjectId: (projectId) => set({ projectId }) }), { name: "streamkit.project" }),
);

export const meKey = ["me"] as const;

export function useMe() {
  return useQuery<Me, ApiError>({ queryKey: meKey, queryFn: ({ signal }) => api<Me>("/me", { signal }), staleTime: 30_000 });
}

export interface ProjectContextValue {
  me: Me;
  project: Project;
  projectId: string;
  /** Owners and admins can manage keys, webhooks, project settings and see the audit log. */
  canAdmin: boolean;
  role: string;
}

export const ProjectContext = React.createContext<ProjectContextValue | null>(null);

/** Only for pages inside the console, where the gate has already found a project. */
export function useProject(): ProjectContextValue {
  const value = React.useContext(ProjectContext);
  if (!value) throw new Error("useProject outside the console");
  return value;
}

export function useProjectQuery<T>(key: readonly unknown[], path: string, options?: Omit<UseQueryOptions<T, ApiError>, "queryKey" | "queryFn">) {
  const { projectId } = useProject();
  return useQuery<T, ApiError>({
    queryKey: [projectId, ...key],
    queryFn: ({ signal }) => api<T>(path, { project: projectId, signal }),
    ...options,
  });
}

export function useProjectApi() {
  const { projectId } = useProject();
  return React.useCallback(<T = unknown>(path: string, opts: Omit<RequestOptions, "project"> = {}) => api<T>(path, { ...opts, project: projectId }), [projectId]);
}

/** PATCH replaces the whole settings object, so changing one part has to send the others back. */
export function useUpdateSettings() {
  const { project } = useProject();
  const qc = useQueryClient();
  return useMutation<Project, ApiError, Partial<ProjectSettings>>({
    mutationFn: (partial) => {
      const settings: ProjectSettings = { ...project.settings, ...partial };
      return api<Project>(`/projects/${project.id}`, { method: "PATCH", body: { settings } });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: meKey }),
  });
}

export function useSignOut() {
  const qc = useQueryClient();
  return React.useCallback(async () => {
    try {
      await api("/auth/logout", { method: "POST" });
    } catch {
      // The session may already be gone. Either way, leave.
    }
    useLive.getState().stop();
    qc.clear();
    window.location.assign("/login");
  }, [qc]);
}
