// App.test.tsx — the admin-reset fence's route gate: behind mustChange
// only the profile (its password form) and the login round-trip stay
// reachable; everything else — token and SSH-key pages included —
// redirects until a self-chosen password is set. The render test pins the
// guard's placement too: an early return ABOVE the hooks once changed the
// hook count between renders and crashed React (#300, caught live on the
// demo stack).
import { describe, expect, it, vi, afterEach, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { AppRoutes, mustChangeAllowed } from "./App";
import { LocaleProvider } from "./i18n";
import { ModeProvider } from "./mode";
import { SessionProvider } from "./session";

describe("mustChangeAllowed", () => {
  it("allows the profile page and the login round-trip", () => {
    expect(mustChangeAllowed("/settings/profile")).toBe(true);
    expect(mustChangeAllowed("/login")).toBe(true);
    expect(mustChangeAllowed("/login?back=%2F")).toBe(true);
  });
  it("blocks repo pages, settings siblings, and the mint faces", () => {
    expect(mustChangeAllowed("/")).toBe(false);
    expect(mustChangeAllowed("/r/demo")).toBe(false);
    expect(mustChangeAllowed("/settings/tokens")).toBe(false);
    expect(mustChangeAllowed("/settings/ssh-keys")).toBe(false);
    expect(mustChangeAllowed("/settings")).toBe(false); // redirects to profile anyway
  });
});

function stubMatchMedia(): void {
  // jsdom ships no matchMedia; the theme code only reads it
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    value: (q: string) => ({ matches: false, media: q, addListener(): void {}, removeListener(): void {}, addEventListener(): void {}, removeEventListener(): void {} }),
  });
}

function stubAPI(session: Record<string, unknown> | null): void {
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes("/api/session")) {
      return new Response(session === null ? null : JSON.stringify(session), {
        status: session === null ? 401 : 200,
        headers: { "Content-Type": "application/json" },
      });
    }
    if (url.includes("/api/mode")) {
      return new Response(
        JSON.stringify({
          profile: "standalone", sso: false, providers: [],
          capabilities: ["groups", "settings", "tokens", "sshKeys", "mergeRequests", "branches", "code", "history", "createRepos"],
        }),
        { headers: { "Content-Type": "application/json" } }
      );
    }
    return new Response("{}", { headers: { "Content-Type": "application/json" } });
  }));
}

function tree(): React.ReactElement {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <QueryClientProvider client={qc}>
      <LocaleProvider>
        <ModeProvider>
          <SessionProvider>
            <MemoryRouter initialEntries={["/r/demo"]}>
              <AppRoutes />
            </MemoryRouter>
          </SessionProvider>
        </ModeProvider>
      </LocaleProvider>
    </QueryClientProvider>
  );
}

describe("the fence in the router", () => {
  afterEach(() => vi.unstubAllGlobals());
  beforeEach(() => stubMatchMedia());

  it("redirects a must-change session to the profile page without crashing", async () => {
    stubAPI({ user: "alice", sso: false, mustChange: true });
    render(tree());
    // lands on the profile page and shows the explaining banner — and the
    // render itself is the regression: a guard above the hooks crashed it
    await waitFor(() => expect(screen.getByText(/reset by an administrator/i)).toBeTruthy());
    expect(screen.getByRole("button", { name: /change password/i })).toBeTruthy();
  });

  it("leaves a clean session on its route", async () => {
    stubAPI({ user: "alice", sso: false });
    render(tree());
    // the repo home rendered (refs stub answers {} → the empty-branch note)
    await waitFor(() => expect(screen.getByText(/no branches yet/i)).toBeTruthy());
  });
});
