// imports.ts — the import face: job records from GET /api/imports and the
// pure name suggestion shared with the form.
export interface ImportJob {
  id: string;
  repo: string;
  sourceUrl: string;
  initiator: string;
  status: "running" | "succeeded" | "failed" | string;
  error?: string;
  refs: number;
  defaultBranch?: string;
  startedAt: number;
  finishedAt: number;
}

/** suggestName derives a single-segment target from the source URL's last
 *  path segment ("owner/repo.git" → "repo"). */
export function suggestName(url: string): string {
  let path = url.trim();
  try {
    path = new URL(url).pathname;
  } catch {
    // bare "owner/repo.git" shapes fall through to the segment walk
  }
  path = path.replace(/\/+$/, "").replace(/\.git$/, "");
  return path.split("/").filter(Boolean).pop() || "";
}
