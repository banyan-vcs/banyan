// ui.tsx — small shared UI atoms.
import type { ReactNode } from "react";
import { useT } from "../i18n";

export function Spinner({ label }: { label?: string }): ReactNode {
  const t = useT();
  return <p className="muted pad">{label ?? t("loading…")}</p>;
}

export function ErrorBox({ error }: { error: unknown }): ReactNode {
  const t = useT();
  const msg = error instanceof Error ? error.message : String(error);
  return <div className="error-box">{t("something went wrong: {0}", msg)}</div>;
}

export function Empty({ children }: { children: ReactNode }): ReactNode {
  return <p className="muted pad">{children}</p>;
}

export function Pill({ children, tone = "normal" }: { children: ReactNode; tone?: "normal" | "warn" | "ok" }): ReactNode {
  return <span className={`pill${tone === "normal" ? "" : " " + tone}`}>{children}</span>;
}

export function whenStr(unix?: number | string): string {
  const n = typeof unix === "string" ? parseInt(unix, 10) : unix || 0;
  if (!n) return "";
  return new Date(n * 1000).toISOString().slice(0, 16).replace("T", " ") + " UTC";
}

export function sizeStr(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
}

export function shortOid(oid: string): string {
  return oid.slice(0, 8);
}
