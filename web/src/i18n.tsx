// i18n.tsx — the bilingual console (Chinese / English). The storage model
// mirrors theme.ts: a three-state choice ("auto" follows the browser,
// which is the default; "zh" and "en" force) persisted in localStorage as
// "banyan.locale", with "auto" stored by removing the key. The dictionary
// strategy is English-source-as-key: t("log in") reads naturally, English
// needs no dictionary (the key IS the text), and zh.ts carries the only
// table — a missing entry degrades to English rather than breaking.
// Positional {n} placeholders allow zh to reorder ("merged as {0} by {1}"
// → "已由 {1} 合并为 {0}"); args may be React nodes, in which case t
// returns a fragment instead of a string (two overloads below).
import { createContext, Fragment, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { zh } from "./zh";

export type Locale = "zh" | "en";
export type LocaleChoice = "auto" | Locale;

const KEY = "banyan.locale";

export function storedLocaleChoice(): LocaleChoice {
  try {
    const v = localStorage.getItem(KEY);
    return v === "zh" || v === "en" ? v : "auto";
  } catch {
    return "auto";
  }
}

// browserLocale scans navigator.languages then navigator.language: any
// zh-prefixed tag (zh, zh-CN, zh-TW…) picks Chinese, everything else
// English — the two languages the console ships.
export function browserLocale(): Locale {
  if (typeof navigator === "undefined") return "en";
  const tags = [...(navigator.languages || []), navigator.language || ""];
  return tags.some((t) => typeof t === "string" && t.toLowerCase().startsWith("zh")) ? "zh" : "en";
}

export function effectiveLocale(choice: LocaleChoice = storedLocaleChoice()): Locale {
  return choice === "auto" ? browserLocale() : choice;
}

/** fmt — splice positional {n} args into a template. All-string args
 * return a plain string (attribute-safe); any ReactNode arg returns a
 * keyed fragment array. An argumentless {n} stays literal — a visible
 * bug marker, not a silent drop. */
export function fmt(template: string, args: unknown[]): unknown {
  if (args.length === 0) return template;
  const pieces = template.split(/(\{\d+\})/g).filter((p) => p !== "");
  const allPrimitive = args.every((a) => typeof a === "string" || typeof a === "number");
  if (allPrimitive) {
    return pieces
      .map((p) => {
        const m = /^\{(\d+)\}$/.exec(p);
        return m ? String(args[Number(m[1])] ?? p) : p;
      })
      .join("");
  }
  return pieces.map((p, i) => {
    const m = /^\{(\d+)\}$/.exec(p);
    return <Fragment key={i}>{m ? ((args[Number(m[1])] ?? p) as ReactNode) : p}</Fragment>;
  });
}

export function translate(locale: Locale, key: string, ...args: unknown[]): unknown {
  const template = (locale === "zh" ? zh[key] : undefined) ?? key;
  return fmt(template, args);
}

/** Tf — the translator signature: string args (or none) yield a string
 * for attributes and plain text; node args yield a renderable. */
export interface Tf {
  (key: string, ...args: (string | number)[]): string;
  (key: string, ...args: ReactNode[]): ReactNode;
}

interface LocaleState {
  choice: LocaleChoice;
  locale: Locale;
  setChoice: (c: LocaleChoice) => void;
}

const LocaleCtx = createContext<LocaleState>({ choice: "auto", locale: "en", setChoice: () => {} });

export function LocaleProvider({ children }: { children: ReactNode }): ReactNode {
  const [choice, setChoiceState] = useState<LocaleChoice>(storedLocaleChoice);
  const locale = choice === "auto" ? browserLocale() : choice;
  // Keep <html lang> honest for screen readers and CJK font selection.
  useEffect(() => {
    document.documentElement.lang = locale === "zh" ? "zh-CN" : "en";
  }, [locale]);
  const setChoice = useCallback((c: LocaleChoice): void => {
    try {
      if (c === "auto") localStorage.removeItem(KEY);
      else localStorage.setItem(KEY, c);
    } catch {
      /* storage unavailable — the in-page switch still works for this view */
    }
    setChoiceState(c);
  }, []);
  return <LocaleCtx.Provider value={{ choice, locale, setChoice }}>{children}</LocaleCtx.Provider>;
}

export function useLocale(): LocaleState {
  return useContext(LocaleCtx);
}

/** useT — the translator hook: re-renders every consumer when the locale
 * flips (the context value changes), so a settings-page switch re-texts
 * the whole tree at once. */
export function useT(): Tf {
  const { locale } = useLocale();
  return useCallback(
    ((key: string, ...args: unknown[]) => translate(locale, key, ...args)) as Tf,
    [locale]
  );
}
