// theme.ts — the three-state theme control behind the settings area's
// Profile page (appearance section): auto (follow the OS scheme) / light / dark. The
// choice persists in localStorage as "banyan.theme"; a forced scheme rides
// html[data-theme], which outranks the prefers-color-scheme block by
// specificity. The index.html pre-paint snippet applies the stored choice
// before first paint.
import { useEffect, useState } from "react";

export type ThemeChoice = "auto" | "light" | "dark";

const KEY = "banyan.theme";

export function storedTheme(): ThemeChoice {
  try {
    const v = localStorage.getItem(KEY);
    return v === "light" || v === "dark" ? v : "auto";
  } catch {
    return "auto";
  }
}

export function systemDark(): boolean {
  return typeof window !== "undefined" && !!window.matchMedia?.("(prefers-color-scheme: dark)").matches;
}

export function effectiveDark(): boolean {
  const t = storedTheme();
  if (t !== "auto") return t === "dark";
  return systemDark();
}

// setTheme is the settings picker's write: pick one of the three states
// outright (the toggle's cycling order is gone with the toggle itself).
export function setTheme(choice: ThemeChoice): void {
  const el = document.documentElement;
  if (choice === "light" || choice === "dark") el.setAttribute("data-theme", choice);
  else el.removeAttribute("data-theme");
  try {
    if (choice === "auto") localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, choice);
  } catch {
    /* storage unavailable — the in-page switch still works for this view */
  }
}

// useEffectiveDark answers whether the console currently renders its dark
// scheme, and keeps answering as the answer changes — the OS scheme flips
// under "auto", and the picker's write lands on html[data-theme] without a
// reload. Third-party surfaces that cannot read our CSS tokens (the diff
// view's own data-theme attribute) ride this hook.
export function useEffectiveDark(): boolean {
  const [dark, setDark] = useState(effectiveDark);
  useEffect(() => {
    const update = (): void => setDark(effectiveDark());
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    mq.addEventListener("change", update);
    // the picker writes both localStorage and the attribute; watching the
    // attribute catches every path that changes the answer
    const obs = new MutationObserver(update);
    obs.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    update();
    return () => {
      mq.removeEventListener("change", update);
      obs.disconnect();
    };
  }, []);
  return dark;
}
