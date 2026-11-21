// theme.ts — the three-state theme control behind the topbar toggle:
// auto (follow the OS scheme) / light / dark. The choice persists in
// localStorage as "banyan.theme"; a forced scheme rides html[data-theme],
// which outranks the prefers-color-scheme block by specificity. The click
// order is auto → the opposite of what is effective → the remaining one →
// auto, so three clicks always come home.
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

function apply(choice: ThemeChoice): void {
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

export function nextTheme(): ThemeChoice {
  const t = storedTheme();
  if (t === "auto") return effectiveDark() ? "light" : "dark";
  if (t === "light") return "dark";
  return "auto";
}

export function cycleTheme(): ThemeChoice {
  const next = nextTheme();
  apply(next);
  return next;
}
