// types.ts — the /api/v1 shapes the console consumes.

export interface RepoInfo {
  id: string;
  defaultBranch: string;
}

export interface RefEntry {
  name: string; // HEAD | refs/heads/<b> | refs/tags/<t> | refs/<ns>/...
  oid: string;
}

export interface LogEntry {
  oid: string;
  subject: string;
  author: string;
  when: number;
  parents: number;
  parentOids?: string[];
}

export interface TreeEntryItem {
  name: string;
  type: "tree" | "blob" | "commit";
  mode: string;
  oid: string;
  size?: number;
}

export interface TreeResponse {
  repo: string;
  rev?: string;
  path?: string;
  count: number;
  entries: TreeEntryItem[];
}

export interface LogResponse {
  repo: string;
  rev?: string;
  path?: string;
  from: string;
  count: number;
  entries: LogEntry[];
}

export interface SubtreeBranch {
  name: string;
  head: string;
  subject?: string;
  author?: string;
  when?: number;
}

export interface SubtreeSummary {
  path: string;
  projected: boolean;
  branches: SubtreeBranch[];
}

export interface SubtreeListing {
  repo: string;
  defaultBranch: string;
  subtrees: SubtreeSummary[];
}

export interface SubtreeRule {
  path: string;
  users: string[];
}

export interface TokenInfo {
  hash: string;
  user: string;
  name: string;
  scope: string;
  expires: string;
  revoked: boolean;
  created: string;
}
