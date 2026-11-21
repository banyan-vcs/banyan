// ProfilePage.tsx — the settings area's Profile section (/settings/profile):
// the appearance section (the theme lives here now — the three states are
// explicit choices, no cycling), the language section (the bilingual
// console's other three-state control: auto / 中文 / English), and the
// password change (server-managed accounts; config-file accounts get the
// explaining refusal). The account's credentials live in their own
// sections — SSH Keys and Access Tokens — behind the settings sidebar.
// Everything here is per-user, never repo-scoped.
import { useEffect, useState, type ReactNode } from "react";
import { useMutation } from "@tanstack/react-query";
import { api } from "../api/client";
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

/** PasswordSection: the server-managed account's self-service. The current
 * credential is re-verified server-side; config-file accounts answer with
 * the explaining refusal (their passwords are not managed here). On
 * success the session's stored credential is stale — the banner says so
 * and the next API call routes to the login page (the password change is
 * the one action that logs you out). */
function PasswordSection(): ReactNode {
  const t = useT();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [error, setError] = useState("");
  const [done, setDone] = useState(false);
  const change = useMutation({
    mutationFn: () =>
      api("/api/v1/auth/password", {
        method: "POST",
        body: JSON.stringify({ current, next }),
      }),
    onSuccess: () => {
      setDone(true);
      setError("");
      setCurrent("");
      setNext("");
    },
    onError: (e) => {
      setDone(false);
      setError(e instanceof Error ? e.message : String(e));
    },
  });
  return (
    <section>
      <h2>{t("Password")}</h2>
      <p className="muted">{t("changing the password ends this session — log in again with the new one.")}</p>
      {error && <div className="error-box">{error}</div>}
      {done && <div className="ok-box">{t("password changed — log in again.")}</div>}
      <form
        className="mint-form"
        onSubmit={(e) => {
          e.preventDefault();
          change.mutate();
        }}
      >
        <input type="password" placeholder={t("current password")} value={current} onChange={(e) => setCurrent(e.target.value)} />
        <input type="password" placeholder={t("new password (8+ characters)")} value={next} onChange={(e) => setNext(e.target.value)} />
        <button className="btn" type="submit" disabled={change.isPending || current === "" || next === ""}>
          {t("change password")}
        </button>
      </form>
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
      <PasswordSection />
    </>
  );
}
