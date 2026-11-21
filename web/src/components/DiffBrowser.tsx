// DiffBrowser.tsx — the console's shared GitHub-style diff surface on
// @git-diff-view/react (lowlight / highlight.js — the same engine the blob
// view rides, no WASM, fully bundled). A toolbar carries the whole-change
// stat and the split/unified toggle (when the caller manages the view);
// one collapsible section per file with its own diffstat. The MR change
// view (MrDiff) passes its inline-edit affordance through fileExtra; the
// per-file section classes stay .mr-diff* — one skin, two surfaces.
import { DiffModeEnum, DiffView } from "@git-diff-view/react";
import "@git-diff-view/react/styles/diff-view.css";
import { useState, type ReactNode } from "react";
import type { DiffViewFile } from "../api/diffview";
import { useT } from "../i18n";

export function DiffBrowser({
  files,
  view,
  onViewChange,
  fileExtra,
  editingPath,
  emptyHint,
}: {
  files: DiffViewFile[];
  view: "split" | "unified";
  onViewChange?: (v: "split" | "unified") => void;
  fileExtra?: (f: DiffViewFile, path: string) => ReactNode;
  editingPath?: string;
  emptyHint?: string;
}): ReactNode {
  const t = useT();
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  if (files.length === 0) {
    return <p className="muted pad">{emptyHint || t("no changes")}</p>;
  }
  const add = files.reduce((n, f) => n + f.add, 0);
  const del = files.reduce((n, f) => n + f.del, 0);
  return (
    <div className="mr-diff">
      <div className="diff-tool">
        <span className="muted">{t("{0} file(s) changed", files.length)}</span>
        <span className="diffstat">
          <span className="add">+{add}</span> <span className="del">−{del}</span>
        </span>
        {onViewChange && (
          <span className="tab-row">
            <button className={"tab" + (view === "split" ? " active" : "")} onClick={() => onViewChange("split")}>
              {t("split")}
            </button>
            <button className={"tab" + (view === "unified" ? " active" : "")} onClick={() => onViewChange("unified")}>
              {t("unified")}
            </button>
          </span>
        )}
      </div>
      {files.map((f, i) => {
        const key = `${f.newPath}#${i}`;
        const path = f.newPath === "/dev/null" ? f.oldPath : f.newPath;
        const name = f.oldPath === "/dev/null" || f.oldPath === f.newPath ? f.newPath : `${f.oldPath} → ${f.newPath}`;
        const editing = editingPath === path;
        return (
          <section key={key} className={"mr-diff-file" + (editing ? " editing" : "")}>
            <div className="mr-diff-head">
              <button className="mr-diff-toggle" onClick={() => setCollapsed((c) => ({ ...c, [key]: !c[key] }))}>
                <span className="mono">{name}</span>
                <span className="muted">
                  {f.binary ? t("binary") : f.newPath === "/dev/null" ? t("deleted") : t("{0} hunk(s)", f.hunks.length)}
                </span>
              </button>
              {!f.binary && (
                <span className="diffstat">
                  <span className="add">+{f.add}</span> <span className="del">−{f.del}</span>
                </span>
              )}
              {fileExtra?.(f, path)}
            </div>
            {!collapsed[key] && !f.binary && (
              <DiffView
                data={{
                  oldFile: { fileName: f.oldPath, fileLang: f.lang, content: null },
                  newFile: { fileName: f.newPath, fileLang: f.lang, content: null },
                  hunks: f.hunks,
                }}
                diffViewMode={view === "unified" ? DiffModeEnum.Unified : DiffModeEnum.Split}
                diffViewWrap={false}
                diffViewFontSize={13}
              />
            )}
          </section>
        );
      })}
    </div>
  );
}
