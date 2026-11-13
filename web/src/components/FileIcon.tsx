// FileIcon.tsx — a small inline glyph per entry kind: a folder for trees
// (accent-tinted via .entry-icon.tree), a page for blobs, an arrow for
// links and a box for submodules.
import type { ReactNode } from "react";
import { FileIcon16, FolderIcon, LinkFileIcon, SubmoduleIcon } from "./icons";

export function FileIcon({ kind, mode }: { name: string; kind: "tree" | "blob" | "commit"; mode: string }): ReactNode {
  if (kind === "tree") return <FolderIcon />;
  if (mode === "160000") return <SubmoduleIcon />;
  if (mode === "120000") return <LinkFileIcon />;
  return <FileIcon16 />;
}
