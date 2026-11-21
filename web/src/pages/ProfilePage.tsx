// ProfilePage.tsx — personal settings: the appearance section (the theme
// lives here now — the three states are explicit choices, no cycling) and,
// when the deployment exposes the token layer, the access-token block that
// used to be its own page. Everything here is per-user, never repo-scoped.
import { useEffect, useState, type ReactNode } from "react";
import { can, useMode } from "../mode";
import { effectiveDark, setTheme, storedTheme, type ThemeChoice } from "../theme";
import { TokensSection } from "../components/TokensSection";

const CHOICES: { value: ThemeChoice; label: string; hint: string }[] = [
  { value: "auto", label: "follow the system", hint: "day or night, whatever the OS says" },
  { value: "light", label: "day", hint: "the monochrome: black primary on white" },
  { value: "dark", label: "night", hint: "warm-dark surfaces, the yellow primary" },
];

function AppearanceSection(): ReactNode {
  // The choice is local state mirrored to localStorage + html[data-theme];
  // a system-scheme flip while "auto" is chosen re-renders the hint only.
  const [choice, setChoice] = useState<ThemeChoice>(storedTheme);
  const [sysDark, setSysDark] = useState(effectiveDark);
  useEffect(() => {
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = (): void => setSysDark(mq.matches);
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);
  return (
    <section>
      <h2>appearance</h2>
      <div className="theme-picker">
        {CHOICES.map((c) => (
          <button
            key={c.value}
            className={"btn" + (choice === c.value ? "" : " ghost")}
            onClick={() => {
              setTheme(c.value);
              setChoice(c.value);
            }}
          >
            {c.label}
          </button>
        ))}
      </div>
      <p className="muted">
        {choice === "auto"
          ? `following the system — currently ${sysDark ? "night" : "day"}`
          : choice === "light"
            ? CHOICES[1].hint
            : CHOICES[2].hint}
      </p>
    </section>
  );
}

export function ProfilePage(): ReactNode {
  const mode = useMode();
  return (
    <>
      <h1>personal settings</h1>
      <AppearanceSection />
      {can(mode, "tokens") && <TokensSection />}
    </>
  );
}
