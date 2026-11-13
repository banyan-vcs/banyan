// client.ts — the SPA's single API face. Every request is same-origin and
// rides the session cookie; the BFF injects the real credentials upstream.
// BASE_URL is the deployment prefix (vite bakes BASE_PATH in: "" or /-/banyan).

export const BASE_URL = import.meta.env.BASE_URL;

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string
  ) {
    super(message);
  }
}

function url(path: string): string {
  return BASE_URL + path.replace(/^\/+/, "");
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = {};
  if (init?.body !== undefined) headers["Content-Type"] = "application/json";
  const resp = await fetch(url(path), { credentials: "same-origin", ...init, headers });
  if (resp.status === 401) {
    onUnauthorized();
    throw new ApiError(401, "not logged in");
  }
  if (!resp.ok) {
    const text = await resp.text().catch(() => "");
    throw new ApiError(resp.status, text || resp.statusText);
  }
  const ct = resp.headers.get("content-type") || "";
  if (ct.includes("application/json")) return (await resp.json()) as T;
  return (await resp.arrayBuffer()) as unknown as T;
}

// onUnauthorized asks the SPA to route to /login (App.tsx listens and
// navigates in-router — no full page reload, no redirect loop on the
// login page itself).
export function onUnauthorized(): void {
  const back =
    window.location.pathname.slice(new URL(BASE_URL, window.location.origin).pathname.length - 1) +
    window.location.search;
  window.dispatchEvent(new CustomEvent("banyan:unauthorized", { detail: { back } }));
}

// apiText fetches text/plain endpoints (diff, blame).
export async function apiText(path: string): Promise<string> {
  const resp = await fetch(url(path), { credentials: "same-origin" });
  if (resp.status === 401) {
    onUnauthorized();
    throw new ApiError(401, "not logged in");
  }
  if (!resp.ok) {
    const text = await resp.text().catch(() => "");
    throw new ApiError(resp.status, text || resp.statusText);
  }
  return resp.text();
}

export function qs(params: Record<string, string | number | undefined>): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== "") q.set(k, String(v));
  }
  const s = q.toString();
  return s === "" ? "" : "?" + s;
}
