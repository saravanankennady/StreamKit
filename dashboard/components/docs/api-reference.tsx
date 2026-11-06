"use client";

import { useQuery } from "@tanstack/react-query";
import * as React from "react";
import { parse } from "yaml";
import { ErrorState } from "@/components/console/page";
import { Tag } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { api, ApiError } from "@/lib/api";
import { cn } from "@/lib/utils";

// Rendered from the server's own spec at /api/v1/openapi.yaml, so it describes the server you are talking to.

interface Parameter {
  name?: string;
  in?: string;
  required?: boolean;
  description?: string;
  schema?: Schema;
  $ref?: string;
}
interface Schema {
  type?: string | string[];
  format?: string;
  enum?: string[];
  description?: string;
  properties?: Record<string, Schema>;
  items?: Schema;
  required?: string[];
  $ref?: string;
  additionalProperties?: unknown;
}
interface Response {
  description?: string;
  $ref?: string;
  content?: Record<string, { schema?: Schema }>;
}
interface Operation {
  tags?: string[];
  summary?: string;
  security?: Record<string, unknown>[];
  parameters?: Parameter[];
  responses?: Record<string, Response>;
}
interface Spec {
  info?: { title?: string; version?: string; description?: string };
  servers?: { url: string }[];
  tags?: { name: string }[];
  paths?: Record<string, Record<string, Operation | Parameter[]>>;
  components?: { parameters?: Record<string, Parameter>; responses?: Record<string, Response>; schemas?: Record<string, Schema> };
}

const METHODS = ["get", "post", "patch", "put", "delete"];

/** The spec's description is Markdown, but only ever uses backticks. */
function withInlineCode(text: string) {
  return text.split("`").map((part, i) => (i % 2 === 1 ? <code key={i} className="mono rounded-xs bg-sunken px-1">{part}</code> : part));
}

function refName(ref?: string) {
  return ref?.split("/").pop() ?? "";
}

function typeLabel(s?: Schema): string {
  if (!s) return "";
  if (s.$ref) return refName(s.$ref);
  if (s.enum) return s.enum.join(" | ");
  if (Array.isArray(s.type)) return s.type.filter((t) => t !== "null").join(" | ") + (s.type.includes("null") ? ", or null" : "");
  if (s.type === "array") return `${typeLabel(s.items)} list`;
  if (s.format) return `${s.type}, ${s.format}`;
  return s.type ?? "object";
}

function SchemaTable({ schema }: { schema: Schema }) {
  const props = Object.entries(schema.properties ?? {});
  if (props.length === 0) return <p className="text-sm text-ink-3">{typeLabel(schema)}</p>;
  return (
    <div className="scroll-quiet overflow-x-auto">
      <table className="w-full text-sm">
        <tbody>
          {props.map(([name, p]) => (
            <tr key={name} className="border-b last:border-b-0">
              <td className="w-48 py-1 pr-4 align-top whitespace-nowrap">
                <code className="mono">{name}</code>
              </td>
              <td className="w-64 py-1 pr-4 align-top text-xs whitespace-nowrap text-ink-3">{typeLabel(p)}</td>
              <td className="py-1 align-top text-ink-2">{p.description}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const methodClass: Record<string, string> = {
  get: "border-line-2 text-ink-2",
  post: "border-ink text-ink",
  patch: "border-ink text-ink",
  put: "border-ink text-ink",
  delete: "border-critical/60 text-critical-text",
};

function OperationRow({ path, method, op, shared, spec }: { path: string; method: string; op: Operation; shared: Parameter[]; spec: Spec }) {
  const resolveParam = (p: Parameter): Parameter => (p.$ref ? (spec.components?.parameters?.[refName(p.$ref)] ?? p) : p);
  const params = [...shared, ...(op.parameters ?? [])].map(resolveParam);
  const auth = (op.security ?? []).flatMap((s) => Object.keys(s));
  const responses = Object.entries(op.responses ?? {});
  return (
    <details className="group border-b last:border-b-0" data-testid="api-operation" data-path={path} data-method={method}>
      <summary className="flex cursor-pointer flex-wrap items-baseline gap-x-3 gap-y-1 px-3 py-2 hover:bg-sunken">
        <span className={cn("mono w-14 shrink-0 rounded-xs border px-1 py-px text-center text-xs uppercase", methodClass[method])}>{method}</span>
        <code className="mono min-w-0 text-sm break-all">{path}</code>
        <span className="min-w-48 flex-1 text-sm text-ink-2">{op.summary}</span>
      </summary>
      <div className="flex flex-col gap-3 border-t bg-sunken px-3 py-3 text-sm">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-xs text-ink-3">Authentication</span>
          {auth.length === 0 ? <Tag>None</Tag> : auth.map((a) => <Tag key={a}>{a === "session" ? "Session cookie" : "Access token"}</Tag>)}
        </div>
        {params.length > 0 && (
          <div>
            <div className="mb-1 text-xs text-ink-3">Parameters</div>
            <div className="scroll-quiet overflow-x-auto">
              <table className="w-full text-sm">
                <tbody>
                  {params.map((p) => (
                    <tr key={`${p.in}-${p.name}`} className="border-b last:border-b-0">
                      <td className="py-1 pr-4 align-top whitespace-nowrap">
                        <code className="mono">{p.name}</code>
                        {p.required && <span className="ml-1 text-xs text-ink-3">required</span>}
                      </td>
                      <td className="py-1 pr-4 align-top text-xs whitespace-nowrap text-ink-3">{p.in}</td>
                      <td className="py-1 pr-4 align-top text-xs text-ink-3">{typeLabel(p.schema)}</td>
                      <td className="py-1 align-top text-ink-2">{p.description}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        )}
        {responses.length > 0 && (
          <div>
            <div className="mb-1 text-xs text-ink-3">Responses</div>
            <ul className="flex flex-col gap-1">
              {responses.map(([status, r]) => {
                const resolved = r.$ref ? (spec.components?.responses?.[refName(r.$ref)] ?? r) : r;
                const schema = resolved.content?.["application/json"]?.schema;
                return (
                  <li key={status} className="flex flex-wrap items-baseline gap-2">
                    <code className="mono">{status}</code>
                    <span className="text-ink-2">{resolved.description}</span>
                    {schema?.$ref && (
                      <a className="link text-xs" href={`#schema-${refName(schema.$ref)}`}>
                        {refName(schema.$ref)}
                      </a>
                    )}
                  </li>
                );
              })}
            </ul>
          </div>
        )}
      </div>
    </details>
  );
}

export function ApiReference() {
  const [filter, setFilter] = React.useState("");
  const spec = useQuery<Spec, ApiError>({
    queryKey: ["docs", "openapi"],
    queryFn: async ({ signal }) => parse(await api<string>("/openapi.yaml", { signal })) as Spec,
    staleTime: 60_000,
  });

  if (spec.isPending) {
    return (
      <div role="status" aria-label="Loading the API reference" className="flex max-w-4xl flex-col gap-3">
        <Skeleton className="h-7 w-64" />
        <Skeleton className="h-40" />
      </div>
    );
  }
  if (spec.error) return <ErrorState error={spec.error} onRetry={() => spec.refetch()} />;

  const s = spec.data;
  const q = filter.trim().toLowerCase();
  const byTag = new Map<string, { path: string; method: string; op: Operation; shared: Parameter[] }[]>();
  for (const [path, item] of Object.entries(s.paths ?? {})) {
    const shared = (item.parameters as Parameter[] | undefined) ?? [];
    for (const method of METHODS) {
      const op = item[method] as Operation | undefined;
      if (!op) continue;
      if (q && !`${method} ${path} ${op.summary ?? ""}`.toLowerCase().includes(q)) continue;
      const tag = op.tags?.[0] ?? "other";
      if (!byTag.has(tag)) byTag.set(tag, []);
      byTag.get(tag)?.push({ path, method, op, shared });
    }
  }
  const tags = [...(s.tags ?? []).map((t) => t.name), ...[...byTag.keys()].filter((t) => !(s.tags ?? []).some((x) => x.name === t))].filter((t) => byTag.has(t));
  return (
    <div className="flex max-w-5xl flex-col gap-5">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">API reference</h1>
        <p className="mt-1 max-w-3xl text-ink-2">
          {s.info?.title ?? "API"} {s.info?.version}. Read live from <a className="link mono" href="/api/v1/openapi.yaml">/api/v1/openapi.yaml</a> on this server. All paths are under <code className="mono">{s.servers?.[0]?.url}</code>.
        </p>
        {s.info?.description && <p className="mt-2 max-w-3xl text-sm whitespace-pre-line text-ink-2">{withInlineCode(s.info.description)}</p>}
      </div>
      <Input type="search" aria-label="Filter routes" placeholder="Filter by path or summary" className="max-w-sm" value={filter} onChange={(e) => setFilter(e.target.value)} />
      {tags.length === 0 && <p className="text-sm text-ink-3">No route matches that.</p>}
      {tags.map((tag) => (
        <section key={tag}>
          <h2 className="mb-1.5 text-sm font-semibold capitalize">{tag}</h2>
          <div className="rounded-xs border bg-surface">
            {byTag.get(tag)?.map(({ path, method, op, shared }) => (
              <OperationRow key={`${method}-${path}`} path={path} method={method} op={op} shared={shared} spec={s} />
            ))}
          </div>
        </section>
      ))}
      {!q && (
        <section>
          <h2 className="mb-1.5 text-sm font-semibold">Objects</h2>
          <div className="flex flex-col gap-3">
            {Object.entries(s.components?.schemas ?? {}).map(([name, schema]) => (
              <div key={name} id={`schema-${name}`} className="scroll-mt-16 rounded-xs border bg-surface px-3 py-2">
                <h3 className="mb-1 text-sm font-medium">{name}</h3>
                <SchemaTable schema={schema} />
              </div>
            ))}
          </div>
        </section>
      )}
    </div>
  );
}
