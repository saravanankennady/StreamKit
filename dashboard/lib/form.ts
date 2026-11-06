import type { FieldValues, Path, UseFormReturn } from "react-hook-form";
import { ApiError } from "./api";

/**
 * Puts the API's validation errors on the right fields and returns what is left to show in general. Field
 * names from the API can be nested ("settings.allowed_origins"); map names that differ from the form's.
 */
export function applyApiError<T extends FieldValues>(form: UseFormReturn<T>, err: unknown, rename: Record<string, Path<T>> = {}): string | null {
  if (err instanceof ApiError && err.fields) {
    let placed = false;
    for (const [field, message] of Object.entries(err.fields)) {
      const name = rename[field] ?? (field as Path<T>);
      const label = message.charAt(0).toUpperCase() + message.slice(1);
      form.setError(name, { type: "server", message: label.endsWith(".") ? label : `${label}.` });
      placed = true;
    }
    if (placed) return null;
  }
  return err instanceof Error ? err.message : "Something went wrong. Try again.";
}

/** Blank number inputs mean "use the default", which the API expresses by leaving the field out. */
export function optionalNumber(v: string | number | undefined | null): number | undefined {
  if (v === undefined || v === null || v === "") return undefined;
  const n = Number(v);
  return Number.isFinite(n) ? n : undefined;
}
