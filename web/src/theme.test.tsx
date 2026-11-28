// theme.test.tsx — useEffectiveDark, the bridge third-party surfaces ride
// (the diff view's own data-theme attribute): it must follow the stored
// choice, the OS scheme under "auto", and — the live part — a theme write
// that lands on html[data-theme] without a reload.
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { ReactNode } from "react";
import { useEffectiveDark } from "./theme";

let osDark = false;
const listeners = new Set<() => void>();

function stubMatchMedia(): void {
  // jsdom ships no matchMedia; the stub carries a controllable OS answer
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    value: (q: string) => ({
      matches: osDark,
      media: q,
      addListener(): void {},
      removeListener(): void {},
      addEventListener(_: string, fn: () => void): void {
        listeners.add(fn);
      },
      removeEventListener(_: string, fn: () => void): void {
        listeners.delete(fn);
      },
    }),
  });
}

function Probe(): ReactNode {
  const dark = useEffectiveDark();
  return <div data-testid="dark">{String(dark)}</div>;
}

beforeEach(() => {
  stubMatchMedia();
  osDark = false;
  listeners.clear();
  localStorage.removeItem("banyan.theme");
  document.documentElement.removeAttribute("data-theme");
});
afterEach(() => {
  cleanup();
  localStorage.removeItem("banyan.theme");
  document.documentElement.removeAttribute("data-theme");
});

describe("useEffectiveDark", () => {
  it("follows the OS scheme under auto", () => {
    osDark = false;
    render(<Probe />);
    expect(screen.getByTestId("dark").textContent).toBe("false");
    cleanup();
    osDark = true;
    render(<Probe />);
    expect(screen.getByTestId("dark").textContent).toBe("true");
  });

  it("the stored forced choice outranks the OS scheme", () => {
    osDark = true;
    localStorage.setItem("banyan.theme", "light");
    render(<Probe />);
    expect(screen.getByTestId("dark").textContent).toBe("false");
    cleanup();
    osDark = false;
    localStorage.setItem("banyan.theme", "dark");
    render(<Probe />);
    expect(screen.getByTestId("dark").textContent).toBe("true");
  });

  it("a write that lands on html[data-theme] flips it live", async () => {
    osDark = false;
    render(<Probe />);
    expect(screen.getByTestId("dark").textContent).toBe("false");
    localStorage.setItem("banyan.theme", "dark");
    document.documentElement.setAttribute("data-theme", "dark");
    await waitFor(() => expect(screen.getByTestId("dark").textContent).toBe("true"));
  });

  it("an OS scheme flip under auto flips it live", async () => {
    render(<Probe />);
    expect(screen.getByTestId("dark").textContent).toBe("false");
    osDark = true;
    for (const fn of listeners) fn();
    await waitFor(() => expect(screen.getByTestId("dark").textContent).toBe("true"));
  });
});
