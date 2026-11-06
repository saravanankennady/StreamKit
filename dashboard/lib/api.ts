import { PRODUCT_NAME } from "./brand";

// The one way this app talks to the backend. Requests go through the Next.js rewrite to /api/v1.

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly fields?: Record<string, string>;

  constructor(status: number, code: string, message: string, fields?: Record<string, string>) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.fields = fields;
  }

  /** The route itself does not exist on this server, as opposed to a JSON 404 about a missing thing. */
  get routeMissing() {
    return this.status === 404 && this.code === "route_not_found";
  }

  get unreachable() {
    return this.code === "backend_unreachable";
  }
}

export function isRouteMissing(err: unknown): boolean {
  return err instanceof ApiError && err.routeMissing;
}

export function isForbidden(err: unknown): boolean {
  return err instanceof ApiError && err.status === 403;
}

export interface RequestOptions {
  method?: "GET" | "POST" | "PATCH" | "DELETE";
  body?: unknown;
  /** Project ID for the X-Project-ID header. Most routes require it. */
  project?: string | null;
  signal?: AbortSignal;
}

export async function api<T = unknown>(path: string, opts: RequestOptions = {}): Promise<T> {
  const method = opts.method ?? "GET";
  const headers: Record<string, string> = {};
  // The API refuses state-changing requests without this type, including DELETE, as a defence against forms on
  // other sites. It is safe to always send.
  if (method !== "GET") headers["Content-Type"] = "application/json";
  if (opts.project) headers["X-Project-ID"] = opts.project;

  let res: Response;
  try {
    res = await fetch(`/api/v1${path}`, {
      method,
      headers,
      credentials: "same-origin",
      signal: opts.signal,
      body: method === "GET" ? undefined : JSON.stringify(opts.body ?? {}),
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    throw new ApiError(0, "backend_unreachable", "The dashboard could not reach its server. Check your connection and try again.");
  }

  if (res.status === 204) return undefined as T;

  const text = await res.text();
  const contentType = res.headers.get("content-type") ?? "";
  if (contentType.includes("application/json")) {
    let data: unknown;
    try {
      data = JSON.parse(text);
    } catch {
      throw new ApiError(res.status, "bad_response", "The server sent a response that could not be read.");
    }
    if (res.ok) return data as T;
    const e = (data as { error?: { code?: string; message?: string; fields?: Record<string, string> } }).error;
    throw new ApiError(res.status, e?.code ?? "error", e?.message ?? `The request failed with status ${res.status}.`, e?.fields);
  }

  if (res.ok) return text as T;
  // Go's mux answers an unknown route in plain text, which is how a service that is not built shows up.
  if (res.status === 404) throw new ApiError(404, "route_not_found", "This server does not have that route.");
  // Next.js answers 500 in plain text or HTML when the proxied backend refuses the connection.
  if (res.status >= 500) {
    throw new ApiError(res.status, "backend_unreachable", `The ${PRODUCT_NAME} server is not responding. Check that streamkit-server is running, then try again.`);
  }
  throw new ApiError(res.status, "error", `The request failed with status ${res.status}.`);
}

/** Decodes an access token's claims for display. It does not verify anything: the server does that. */
export function decodeJwtClaims(token: string): Record<string, unknown> | null {
  const part = token.split(".")[1];
  if (!part) return null;
  try {
    const json = atob(part.replace(/-/g, "+").replace(/_/g, "/"));
    return JSON.parse(new TextDecoder().decode(Uint8Array.from(json, (c) => c.charCodeAt(0))));
  } catch {
    return null;
  }
}

/** The HTTP(S) origin of a project's ws(s):// address. */
export function httpOrigin(wsUrl: string): string {
  return wsUrl.replace(/^ws(s?):/, "http$1:").replace(/\/+$/, "");
}
