// presetAvatars.test.ts — the preset table's invariants: the visual
// design's 24 faces, each riding the same wire an upload rides (a base64
// SVG data URL under the server's 4000-char cap), URLs distinct and
// round-trippable, and the stylesheet styling the classes the gallery
// emits — the two-sided contract that once split the avatar ladder.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { PRESET_AVATARS, presetAvatarByUrl } from "./presetAvatars";

// meta.MaxAvatarURL (internal/meta/users.go) — the wire cap the store enforces
const CAP = 4000;
const PREFIX = "data:image/svg+xml;base64,";

describe("PRESET_AVATARS", () => {
  it("carries the design's 24 faces, ids 1..24 in the mockup's order", () => {
    expect(PRESET_AVATARS).toHaveLength(24);
    expect(PRESET_AVATARS.map((p) => p.id)).toEqual(Array.from({ length: 24 }, (_, i) => i + 1));
  });

  it("rides the upload wire: standalone SVGs as base64 data URLs under the cap", () => {
    for (const p of PRESET_AVATARS) {
      expect(p.url.startsWith(PREFIX)).toBe(true);
      expect(p.url.length).toBeLessThanOrEqual(CAP);
      // standalone documents need the xmlns the mockup's <symbol> inherited
      const svg = atob(p.url.slice(PREFIX.length));
      expect(svg.startsWith('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"')).toBe(true);
      expect(svg.endsWith("</svg>")).toBe(true);
    }
  });

  it("keeps the 24 faces distinct on the wire and in name", () => {
    expect(new Set(PRESET_AVATARS.map((p) => p.url)).size).toBe(24);
    expect(new Set(PRESET_AVATARS.map((p) => p.title)).size).toBe(24);
  });

  it("recognizes its own URLs and nothing else", () => {
    for (const p of PRESET_AVATARS) {
      expect(presetAvatarByUrl(p.url)?.id).toBe(p.id);
    }
    expect(presetAvatarByUrl("data:image/png;base64,AAAA")).toBeUndefined(); // an upload
    expect(presetAvatarByUrl("")).toBeUndefined(); // the name-initial default
  });

  it("emits the classes the stylesheet styles", () => {
    const css = readFileSync(join(__dirname, "..", "styles.less"), "utf8");
    for (const cls of [".pav-grid", ".pav-item", ".pav-item.sel"]) {
      expect(css).toContain(`${cls} {`);
    }
  });
});
