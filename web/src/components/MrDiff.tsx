// MrDiff.tsx — the MR shell over the shared DiffBrowser: every file that
// exists on the source side carries an edit affordance — the inline edit
// that lands on the source branch right from the change view. The diff
// surface itself (highlighting, diffstat, split/unified, folding) lives in
// DiffBrowser, shared with the commit page.
import type { ReactNode } from "react";
import type { DiffViewFile } from "../api/diffview";
import { DiffBrowser } from "./DiffBrowser";

export function MrDiff({
  files,
  view,
  onViewChange,
  onEdit,
  editingPath,
}: {
  files: DiffViewFile[];
  view: "split" | "unified";
  onViewChange?: (v: "split" | "unified") => void;
  onEdit?: (path: string) => void;
  editingPath?: string;
}): ReactNode {
  return (
    <DiffBrowser
      files={files}
      view={view}
      onViewChange={onViewChange}
      editingPath={editingPath}
      emptyHint="the diff is empty — the source carries nothing new against the merge base."
      fileExtra={
        onEdit
          ? (f, path) => {
              if (f.binary || f.newPath === "/dev/null") return null;
              const editing = editingPath === path;
              return (
                <button className="btn ghost small" onClick={() => onEdit(path)} disabled={editing}>
                  {editing ? "editing below" : "edit"}
                </button>
              );
            }
          : undefined
      }
    />
  );
}
