// TokensPage.tsx — the settings area's Access Tokens section
// (/settings/tokens): the TokensSection block that used to live in the
// profile page, now a section of its own behind the settings sidebar.
import { useT } from "../i18n";
import type { ReactNode } from "react";
import { TokensSection } from "../components/TokensSection";

export function TokensPage(): ReactNode {
  const t = useT();
  return (
    <>
      <h1>{t("Access Tokens")}</h1>
      <TokensSection />
    </>
  );
}
