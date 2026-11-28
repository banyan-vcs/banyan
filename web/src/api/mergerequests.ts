// mergeRequests.ts — the merge-request API client (docs/design/merge-requests.md).
// The wire shapes mirror the gateway's /api/v1/repos/<repo>/merge-requests,
// /branch-rules, and the workspace-wide /api/v1/merge-requests faces.
import { api } from "./client";

export interface MergeRequestInfo {
  repo: string;
  iid: number;
  title: string;
  description?: string;
  source: string;
  target: string;
  state: "open" | "merged" | "closed" | string;
  author: string;
  source_head: string;
  merge_commit: string;
  merged_by: string;
  created_at: number;
  updated_at: number;
}

export interface MergeRequestView extends MergeRequestInfo {
  source_tip: string;
  mergeable: boolean;
  conflicts?: string[];
  merge_base: string;
  approvals: string[];
  required_approvals: number;
  rule_level?: string;
  blocking?: string[]; // reviewers whose latest verdict is request-changes
}

export interface BranchRule {
  repo: string;
  branch: string;
  level: "protected" | "merge-request-only" | string;
  approvals: number;
}

export const listMergeRequests = (repo: string, state = ""): Promise<MergeRequestInfo[]> =>
  api<{ repo: string; merge_requests: MergeRequestInfo[] }>(
    `/api/v1/repos/${encodeURIComponent(repo)}/merge-requests${state ? `?state=${state}` : ""}`,
  ).then((r) => r.merge_requests ?? []);

// listAllMergeRequests — the workspace-wide list (the sidebar's cross-repo view):
// every visible repository's ledger rows in one answer, newest first.
export const listAllMergeRequests = (state = ""): Promise<MergeRequestInfo[]> =>
  api<{ merge_requests: MergeRequestInfo[] }>(`/api/v1/merge-requests${state ? `?state=${state}` : ""}`).then(
    (r) => r.merge_requests ?? [],
  );

export const getMergeRequest = (repo: string, iid: number | string): Promise<MergeRequestView> =>
  api<MergeRequestView>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}`);

export const openMergeRequest = (repo: string, body: { source: string; target: string; title: string; description?: string }): Promise<MergeRequestInfo> =>
  api<MergeRequestInfo>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests`, { method: "POST", body: JSON.stringify(body) });

export const approveMergeRequest = (repo: string, iid: number | string): Promise<unknown> =>
  api(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/approve`, { method: "POST", body: "{}" });

export const unapproveMergeRequest = (repo: string, iid: number | string): Promise<unknown> =>
  api(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/approve`, { method: "DELETE" });

export const mergeMergeRequest = (repo: string, iid: number | string, message = ""): Promise<MergeRequestInfo> =>
  api<MergeRequestInfo>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/merge`, {
    method: "POST",
    body: JSON.stringify({ message }),
  });

export const closeMergeRequest = (repo: string, iid: number | string): Promise<MergeRequestInfo> =>
  api<MergeRequestInfo>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/close`, { method: "POST", body: "{}" });

export const reopenMergeRequest = (repo: string, iid: number | string): Promise<MergeRequestInfo> =>
  api<MergeRequestInfo>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/reopen`, { method: "POST", body: "{}" });

export const branchRules = (repo: string): Promise<BranchRule[]> =>
  api<{ repo: string; rules: BranchRule[] }>(`/api/v1/repos/${encodeURIComponent(repo)}/branch-rules`).then(
    (r) => r.rules ?? [],
  );

export const putBranchRule = (repo: string, branch: string, level: string, approvals: number): Promise<BranchRule> =>
  api<BranchRule>(`/api/v1/repos/${encodeURIComponent(repo)}/branch-rules/${encodeURIComponent(branch)}`, {
    method: "PUT",
    body: JSON.stringify({ level, approvals }),
  });

export const deleteBranchRule = (repo: string, branch: string): Promise<unknown> =>
  api(`/api/v1/repos/${encodeURIComponent(repo)}/branch-rules/${encodeURIComponent(branch)}`, { method: "DELETE" });

// --- the discussion ledger (docs/design/merge-requests.md §review) ---

export interface MergeRequestComment {
  id: number;
  repo: string;
  iid: number;
  parent_id?: number; // 0/absent = thread head
  author: string;
  body: string;
  kind: "comment" | "review" | string;
  verdict?: "approve" | "request-changes" | "comment" | string;
  path?: string; // anchor: repo-relative path
  line?: number; // anchor: line number on side
  side?: "old" | "new" | string; // anchor: which pane
  anchor_oid?: string; // anchor: the source tip the comment read
  resolved: boolean;
  resolved_by?: string;
  created_at: number;
}

export interface MergeRequestApprovalEvent {
  user: string;
  at: number;
}

export interface MergeRequestDiscussion {
  comments: MergeRequestComment[];
  approval_events: MergeRequestApprovalEvent[];
}

export const getDiscussion = (repo: string, iid: number | string): Promise<MergeRequestDiscussion> =>
  api<MergeRequestDiscussion>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/comments`);

export const addMergeRequestComment = (
  repo: string,
  iid: number | string,
  body: { body: string; path?: string; side?: string; line?: number; parent_id?: number },
): Promise<MergeRequestComment> =>
  api<MergeRequestComment>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/comments`, {
    method: "POST",
    body: JSON.stringify(body),
  });

export const resolveMergeRequestComment = (repo: string, iid: number | string, id: number, resolved: boolean): Promise<unknown> =>
  api(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/comments/${id}/resolve`, {
    method: "POST",
    body: JSON.stringify({ resolved }),
  });

export const submitMergeRequestReview = (
  repo: string,
  iid: number | string,
  verdict: "approve" | "request-changes" | "comment",
  body = "",
): Promise<MergeRequestComment> =>
  api<MergeRequestComment>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/review`, {
    method: "POST",
    body: JSON.stringify({ verdict, body }),
  });
