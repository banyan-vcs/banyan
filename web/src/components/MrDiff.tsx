// MrDiff.tsx — the MR diff surface on @git-diff-view/react (lowlight /
// highlight.js syntax highlighting — the same engine the blob view rides,
// no WASM, fully bundled). One collapsible section per file, split or
// unified per the shared ?view state. Files that exist on the source side
// carry an edit affordance — the inline edit that lands on the source
// branch right from the change view.
import { DiffModeEnum, DiffView } from "@git-diff-view/react";
import "@git-diff-view/react/styles/diff-view.css";
import { useState, type ReactNode } from "react";
import type { DiffViewFile } from "../api/diffview";

export function MrDiff({
  files,
  view,
  onEdit,
  editingPath,
}: {
  files: DiffViewFile[];
  view: "split" | "unified";
  onEdit?: (path: string) => void;
  editingPath?: string;
}): ReactNode {
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  if (files.length === 0) {
    return <p className="muted pad">the diff is empty — the source carries nothing new against the merge base.</p>;
  }
  return (
    <div className="mr-diff">
      {files.map((f, i) => {
        const key = `${f.newPath}#${i}`;
        const path = f.newPath === "/dev/null" ? f.oldPath : f.newPath;
        const name = f.oldPath === "/dev/null" || f.oldPath === f.newPath ? f.newPath : `${f.oldPath} → ${f.newPath}`;
        const editable = !!onEdit && !f.binary && f.newPath !== "/dev/null";
        const editing = editingPath === path;
        return (
          <section key={key} className={"mr-diff-file" + (editing ? " editing" : "")}>
            <div className="mr-diff-head">
              <button className="mr-diff-toggle" onClick={() => setCollapsed((c) => ({ ...c, [key]: !c[key] }))}>
                <span className="mono">{name}</span>
                <span className="muted">{f.binary ? "binary" : f.newPath === "/dev/null" ? "deleted" : `${f.hunks.length} hunk(s)`}</span>
              </button>
              {editable && (
                <button className="btn ghost small" onClick={() => onEdit?.(path)} disabled={editing}>
                  {editing ? "editing below" : "edit"}
                </button>
              )}
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
