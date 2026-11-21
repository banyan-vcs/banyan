// i18n.test.tsx — the bilingual core: placeholder splicing (string and
// node args), the English-source-as-key fallback chain, browser-language
// detection, the three-state choice persisting like the theme's, a live
// provider re-render on switch, and the dictionary's placeholder parity
// (a zh value must carry exactly the {n} set its key does, or reordering
// would silently eat an argument).
import { describe, expect, it } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import type { ReactElement } from "react";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";
import { browserLocale, effectiveLocale, fmt, LocaleProvider, storedLocaleChoice, translate, useLocale, useT } from "./i18n";
import { zh } from "./zh";

const spots = (s: string): string[] => (s.match(/\{\d+\}/g) || []).sort();

describe("fmt", () => {
  it("splices string args into a string", () => {
    expect(fmt("rename {0} → {1}?", ["demo", "spark"])).toBe("rename demo → spark?");
  });
  it("keeps an argumentless placeholder literal — a visible marker, not a silent drop", () => {
    expect(fmt("{0} and {1}", ["only"])).toBe("only and {1}");
  });
  it("returns a node array when any arg is a node", () => {
    const out = fmt("no groups yet — {0}; x", [<em key="e">create one</em>]) as unknown[];
    expect(Array.isArray(out)).toBe(true);
  });
});

describe("translate", () => {
  it("hits the zh dictionary and may reorder placeholders", () => {
    expect(translate("zh", "merged as {0} by {1}", "ab12", "alice")).toBe("已由 alice 合并为 ab12");
  });
  it("returns the key itself in English — the source text is the fallback", () => {
    expect(translate("en", "log in")).toBe("log in");
  });
  it("falls back to the key for a zh miss", () => {
    expect(translate("zh", "untranslated string")).toBe("untranslated string");
  });
});

describe("browserLocale", () => {
  it("follows any zh-prefixed tag", () => {
    const def = Object.getOwnPropertyDescriptor(window.navigator, "languages");
    Object.defineProperty(window.navigator, "languages", { value: ["zh-CN", "en-US"], configurable: true });
    expect(browserLocale()).toBe("zh");
    expect(effectiveLocale("auto")).toBe("zh");
    if (def) Object.defineProperty(window.navigator, "languages", def);
  });
  it("defaults to English otherwise", () => {
    const def = Object.getOwnPropertyDescriptor(window.navigator, "languages");
    Object.defineProperty(window.navigator, "languages", { value: ["en-US"], configurable: true });
    expect(browserLocale()).toBe("en");
    if (def) Object.defineProperty(window.navigator, "languages", def);
  });
});

describe("the dictionary", () => {
  it("keeps placeholder parity between every key and its zh value", () => {
    const bad = Object.entries(zh).filter(([k, v]) => spots(k).join() !== spots(v).join());
    expect(bad).toEqual([]);
  });

  /** Every static t("…") key at a call site must carry a zh entry — a
   * miss would silently fall back to English in Chinese mode. Dynamic
   * keys (t(state), t(c.label)…) are the writer's to keep covered. */
  it("translates every static call-site key", () => {
    const keys = new Set<string>();
    const walk = (dir: string): void => {
      for (const f of readdirSync(dir)) {
        const p = join(dir, f);
        if (statSync(p).isDirectory()) walk(p);
        else if (/\.tsx?$/.test(f) && !/\.test\./.test(f)) {
          const text = readFileSync(p, "utf8");
          for (const m of text.matchAll(/\bt\(\s*\n?\s*"((?:[^"\\]|\\.)*)"/g)) keys.add(m[1]);
        }
      }
    };
    walk(__dirname);
    expect([...keys].filter((k) => !(k in zh))).toEqual([]);
  });
});

describe("LocaleProvider", () => {
  it("re-texts the tree when the choice flips, and persists it", () => {
    localStorage.removeItem("banyan.locale");
    function Probe(): ReactElement {
      const t = useT();
      const { choice, setChoice } = useLocale();
      return (
        <div>
          <span data-testid="txt">{t("log in")}</span>
          <span data-testid="choice">{choice}</span>
          <button onClick={() => setChoice("zh")}>flip</button>
        </div>
      );
    }
    // "auto" follows whatever the host browser reports — assert against
    // effectiveLocale rather than assuming a language for the test host.
    const initial = effectiveLocale("auto") === "zh" ? "登录" : "log in";
    const r = render(
      <LocaleProvider>
        <Probe />
      </LocaleProvider>
    );
    expect(screen.getByTestId("txt").textContent).toBe(initial);
    expect(storedLocaleChoice()).toBe("auto");
    fireEvent.click(screen.getByText("flip"));
    expect(screen.getByTestId("txt").textContent).toBe("登录");
    expect(storedLocaleChoice()).toBe("zh");
    expect(document.documentElement.lang).toBe("zh-CN");
    r.unmount();
    localStorage.removeItem("banyan.locale");
  });
});
