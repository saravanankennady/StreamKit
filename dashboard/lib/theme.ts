"use client";

import { create } from "zustand";

export type ThemeChoice = "light" | "dark" | "system";

const KEY = "streamkit.theme";

function apply(choice: ThemeChoice) {
  const dark = choice === "dark" || (choice === "system" && window.matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.dataset.theme = dark ? "dark" : "light";
}

export const useTheme = create<{ choice: ThemeChoice; setChoice: (c: ThemeChoice) => void; hydrate: () => void }>((set) => ({
  choice: "system",
  setChoice(choice) {
    try {
      window.localStorage.setItem(KEY, choice);
    } catch {
      // Private windows can refuse storage. The theme still applies until the page reloads.
    }
    apply(choice);
    set({ choice });
  },
  hydrate() {
    let choice: ThemeChoice = "system";
    try {
      const saved = window.localStorage.getItem(KEY);
      if (saved === "light" || saved === "dark") choice = saved;
    } catch {
      // Keep the default.
    }
    set({ choice });
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
      if (useTheme.getState().choice === "system") apply("system");
    });
  },
}));

/** Runs before first paint, so a dark-mode user never sees a light flash. */
export const THEME_SCRIPT = `(function(){try{var c=localStorage.getItem("${KEY}");var d=c==="dark"||((c!=="light")&&matchMedia("(prefers-color-scheme: dark)").matches);document.documentElement.dataset.theme=d?"dark":"light"}catch(e){document.documentElement.dataset.theme="light"}})()`;
