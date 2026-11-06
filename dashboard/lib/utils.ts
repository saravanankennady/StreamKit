import { clsx, type ClassValue } from "clsx";
import { extendTailwindMerge } from "tailwind-merge";

const merge = extendTailwindMerge({
  extend: { theme: { text: ["2xs", "xs", "sm", "base", "lg", "xl", "2xl", "3xl"] } },
});

export function cn(...inputs: ClassValue[]) {
  return merge(clsx(inputs));
}
