// ThemeToggle.tsx — the topbar's day/night switch: one icon button cycling
// auto (follows the OS) → light → dark → auto. The icon shows the CHOICE
// (half disc = auto); the title spells it out. While in auto the icon
// refreshes live with the OS scheme.
import { useEffect, useState, type ReactNode } from "react";
import { cycleTheme, storedTheme } from "../theme";

function IconAuto(): ReactNode {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.4" aria-hidden="true">
      <circle cx="8" cy="8" r="6" />
      <path d="M8 2a6 6 0 0 1 0 12Z" fill="currentColor" stroke="none" />
    </svg>
  );
}
function IconSun(): ReactNode {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.4" aria-hidden="true">
      <circle cx="8" cy="8" r="3.2" />
      <path d="M8 1v1.8M8 13.2V15M1 8h1.8M13.2 8H15M3 3l1.3 1.3M11.7 11.7 13 13M13 3l-1.3 1.3M4.3 11.7 3 13" />
    </svg>
  );
}
function IconMoon(): ReactNode {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.4" aria-hidden="true">
      <path d="M13.4 9.6A6 6 0 0 1 6.4 2.6a6 6 0 1 0 7 7Z" />
    </svg>
  );
}

const TITLE: Record<"auto" | "light" | "dark", string> = {
  auto: "theme: follow the system (click to switch)",
  light: "theme: day (click to switch)",
  dark: "theme: night (click to switch)",
};

export function ThemeToggle(): ReactNode {
  const [choice, setChoice] = useState<"auto" | "light" | "dark">("auto");
  const [, setSystemTick] = useState(0);

  useEffect(() => {
    setChoice(storedTheme());
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const on = (): void => setSystemTick((n) => n + 1); // auto's icon rides the OS scheme
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, []);

  return (
    <button
      className="theme-btn"
      title={TITLE[choice]}
      aria-label={TITLE[choice]}
      onClick={() => setChoice(cycleTheme())}
    >
      {choice === "dark" ? <IconMoon /> : choice === "light" ? <IconSun /> : <IconAuto />}
    </button>
  );
}
