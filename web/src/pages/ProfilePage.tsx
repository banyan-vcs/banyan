// ProfilePage.tsx — the settings area's Profile section (/settings/profile):
// the appearance section (the theme lives here now — the three states are
// explicit choices, no cycling) and the language section (the bilingual
// console's other three-state control: auto / 中文 / English). The
// account's credentials live in their own sections — SSH Keys and Access
// Tokens — behind the settings sidebar. Everything here is per-user, never
// repo-scoped.
import { useEffect, useState, type ReactNode } from "react";
import { useLocale, useT, type Locale, type LocaleChoice } from "../i18n";
import { effectiveDark, setTheme, storedTheme, type ThemeChoice } from "../theme";

const CHOICES: { value: ThemeChoice; label: string; hint: string }[] = [
  { value: "auto", label: "auto", hint: "day or night, whatever the OS says" },
  { value: "light", label: "day", hint: "the monochrome: black primary on white" },
  { value: "dark", label: "night", hint: "warm-dark surfaces, the yellow primary" },
];

function AppearanceSection(): ReactNode {
  const t = useT();
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
      <h2>{t("Appearance")}</h2>
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
            {t(c.label)}
          </button>
        ))}
      </div>
      <p className="muted">
        {choice === "auto"
          ? t("following the system — currently {0}", sysDark ? t("night") : t("day"))
          : t(CHOICES[choice === "light" ? 1 : 2].hint)}
      </p>
    </section>
  );
}

/** The language's own choices: "auto" translates with the UI; 中文 /
 * English always wear their native names regardless of the current
 * locale — the standard convention for language pickers. */
const LANG_CHOICES: { value: LocaleChoice; label: string }[] = [
  { value: "auto", label: "auto" },
  { value: "zh", label: "中文" },
  { value: "en", label: "English" },
];

function LanguageSection(): ReactNode {
  const t = useT();
  const { choice, locale, setChoice } = useLocale();
  const native = (l: Locale): string => (l === "zh" ? "中文" : "English");
  return (
    <section>
      <h2>{t("Language")}</h2>
      <div className="theme-picker">
        {LANG_CHOICES.map((c) => (
          <button
            key={c.value}
            className={"btn" + (choice === c.value ? "" : " ghost")}
            onClick={() => setChoice(c.value)}
          >
            {c.value === "auto" ? t(c.label) : c.label}
          </button>
        ))}
      </div>
      <p className="muted">
        {choice === "auto"
          ? t("following the browser — currently {0}", native(locale))
          : native(locale)}
      </p>
    </section>
  );
}

export function ProfilePage(): ReactNode {
  const t = useT();
  return (
    <>
      <h1>{t("Profile")}</h1>
      <AppearanceSection />
      <LanguageSection />
    </>
  );
}
