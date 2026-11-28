// ProfilePage.test.tsx — the preset gallery's behavior: the design's 24
// swatches render, a click PUTs the same data: URL an upload would, and
// the selected ring follows the server's answer — never local optimism.
import { cleanup, render, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";
import { LocaleProvider } from "../i18n";
import { SessionProvider } from "../session";
import { AvatarSection } from "./ProfilePage";
import { PRESET_AVATARS } from "../components/presetAvatars";

// the avatar column the stubbed store carries between calls — a PUT moves
// it, the next GET answers with it, exactly like the real seam
let avatarUrl = "";
const puts: { method: string; path: string; avatarUrl: string }[] = [];

function json(body: unknown): Response {
  return new Response(JSON.stringify(body), { headers: { "content-type": "application/json" } });
}

beforeEach(() => {
  avatarUrl = "";
  puts.length = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string | URL, init?: RequestInit): Promise<Response> => {
      const path = String(input).replace(/^https?:\/\/[^/]+/, "");
      const method = init?.method ?? "GET";
      if (path.endsWith("/api/session")) return json({ user: "alice" });
      if (path === "/api/v1/users/alice/avatar") return json({ user: "alice", avatarUrl });
      if (path === "/api/v1/me/avatar") {
        avatarUrl = (JSON.parse(String(init?.body)) as { avatarUrl: string }).avatarUrl;
        puts.push({ method, path, avatarUrl });
        return json({ user: "alice", avatarUrl });
      }
      return json({});
    }),
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
});

function page(): ReactNode {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <QueryClientProvider client={qc}>
      <LocaleProvider>
        <SessionProvider>
          <AvatarSection />
        </SessionProvider>
      </LocaleProvider>
    </QueryClientProvider>
  );
}

describe("the preset gallery", () => {
  it("renders the design's 24 swatches, none selected by default", async () => {
    const { container } = render(page());
    await waitFor(() => expect(container.querySelectorAll(".pav-item")).toHaveLength(24));
    expect(container.querySelectorAll(".pav-item.sel")).toHaveLength(0);
  });

  it("selecting a preset PUTs the very URL the table generated", async () => {
    const fox = PRESET_AVATARS[6]; // animal · fox
    const { container } = render(page());
    const btn = await waitFor(() => {
      const el = container.querySelectorAll(".pav-item")[6] as HTMLButtonElement;
      expect(el).toBeTruthy();
      return el;
    });
    btn.click();
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]).toMatchObject({ method: "PUT", path: "/api/v1/me/avatar", avatarUrl: fox.url });
    // the ring follows the server's answer, and exactly one face wears it
    await waitFor(() => {
      const sel = container.querySelectorAll(".pav-item.sel");
      expect(sel).toHaveLength(1);
      expect(sel[0].querySelector("img")?.getAttribute("src")).toBe(fox.url);
      expect(sel[0].getAttribute("aria-pressed")).toBe("true");
    });
  });

  it("a stored preset arrives selected; clearing deselects it", async () => {
    const panda = PRESET_AVATARS[10]; // animal · panda
    avatarUrl = panda.url;
    const { container } = render(page());
    await waitFor(() => {
      const sel = container.querySelectorAll(".pav-item.sel");
      expect(sel).toHaveLength(1);
      expect(sel[0].querySelector("img")?.getAttribute("src")).toBe(panda.url);
    });
    const clear = container.querySelector('button.btn.ghost:not(.pav-item)') as HTMLButtonElement;
    clear.click();
    await waitFor(() => expect(container.querySelectorAll(".pav-item.sel")).toHaveLength(0));
    expect(puts.at(-1)?.avatarUrl).toBe("");
  });
});
