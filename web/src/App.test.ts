// App.test.ts — the admin-reset fence's route gate: behind mustChange
// only the profile (its password form) and the login round-trip stay
// reachable; everything else — token and SSH-key pages included —
// redirects until a self-chosen password is set.
import { describe, expect, it } from "vitest";
import { mustChangeAllowed } from "./App";

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
