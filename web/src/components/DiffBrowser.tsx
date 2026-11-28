// DiffBrowser.tsx — the console's shared GitHub-style diff surface on
// @git-diff-view/react (lowlight / highlight.js — the same engine the blob
// view rides, no WASM, fully bundled). A toolbar carries the whole-change
// stat and the split/unified toggle (when the caller manages the view);
// one collapsible section per file with its own diffstat. The merge request change
// view (MergeRequestDiff) passes its inline-edit affordance through fileExtra; the
// per-file section classes stay .diff-browser* — one skin, two surfaces.
import { DiffModeEnum, DiffView, SplitSide } from "@git-diff-view/react";
import "@git-diff-view/react/styles/diff-view.css";
import { useState, type ReactNode } from "react";
import type { DiffViewFile } from "../api/diffview";
import { useEffectiveDark } from "../theme";
import { useT } from "../i18n";

// LineWidgetCtx is what a line's widget renderer sees: the pane the "+"
// sat on (old = the pre-image, new = the post-image), the line number in
// that pane's numbering, and the closer the form calls when it is done.
export interface LineWidgetCtx {
  side: "old" | "new";
  lineNumber: number;
  onClose: () => void;
}

export function DiffBrowser({
  files,
  view,
  onViewChange,
  fileExtra,
  editingPath,
  emptyHint,
  renderLineWidget,
}: {
  files: DiffViewFile[];
  view: "split" | "unified";
  onViewChange?: (v: "split" | "unified") => void;
  fileExtra?: (f: DiffViewFile, path: string) => ReactNode;
  editingPath?: string;
  emptyHint?: string;
  // When set, every content line grows the library's hover "+" and clicking
  // it opens renderLineWidget's row at that line (the merge request review's inline
  // comment affordance; the commit page passes nothing and sees none).
  renderLineWidget?: (path: string, ctx: LineWidgetCtx) => ReactNode;
}): ReactNode {
  const t = useT();
  // The library keeps its own data-theme attribute (its built-in light and
  // dark palettes key off it); without the prop it stays "light" forever —
  // the dark console wore a white diff island. The hook follows both the
  // manual override and the OS scheme, live.
  const dark = useEffectiveDark();
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  if (files.length === 0) {
    return <p className="muted pad">{emptyHint || t("no changes")}</p>;
  }
  const add = files.reduce((n, f) => n + f.add, 0);
  const del = files.reduce((n, f) => n + f.del, 0);
  return (
    <div className="diff-browser">
      <div className="diff-tool">
        <span className="muted">{t("{0} file(s) changed", files.length)}</span>
        <span className="diffstat">
          <span className="add">+{add}</span> <span className="del">−{del}</span>
        </span>
        {onViewChange && (
          <span className="tab-row">
            <button
              className={"tab" + (view === "split" ? " active" : "")}
              onClick={() => onViewChange("split")}
              title={t("side-by-side")}
            >
              {t("split")}
            </button>
            <button
              className={"tab" + (view === "unified" ? " active" : "")}
              onClick={() => onViewChange("unified")}
              title={t("inline")}
            >
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
        const isCollapsed = !!collapsed[key];
        return (
          <section key={key} className={"diff-file" + (editing ? " editing" : "") + (isCollapsed ? " collapsed" : "")}>
            <div className="diff-file-head">
              <button
                className="diff-file-toggle"
                onClick={() => setCollapsed((c) => ({ ...c, [key]: !c[key] }))}
                aria-expanded={!isCollapsed}
              >
                <span className="fold-caret" aria-hidden="true">
                  <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
                    <path d="M4 6l4 4 4-4" />
                  </svg>
                </span>
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
                diffViewTheme={dark ? "dark" : "light"}
                // explicit and load-bearing: the component's initSyntax only
                // fires when this prop is truthy — without it hunks render,
                // rows number, but no token ever gets a color
                diffViewHighlight={true}
                diffViewAddWidget={!!renderLineWidget}
                renderWidgetLine={({ side, lineNumber, onClose }) =>
                  renderLineWidget?.(path, {
                    side: side === SplitSide.old ? "old" : "new",
                    lineNumber,
                    onClose,
                  }) ?? null
                }
              />
            )}
          </section>
        );
      })}
    </div>
  );
}
