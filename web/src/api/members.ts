// members.ts — the member ledger's API face (docs/design/data-model.md
// §16): the group table (what labeled repositories inherit) and the
// repository's effective view, inherited rows carrying viaGroup.
import { api } from "./client";

export interface Member {
  user: string;
  rank: string;
  addedBy: string;
  addedAt: number;
}

export interface EffectiveMember extends Member {
  viaGroup: string;
}

export interface GroupMembersResponse {
  group: string;
  members: Member[];
}

export interface RepoMembersResponse {
  repo: string;
  members: EffectiveMember[];
  direct: Member[];
}

export function getGroupMembers(group: string): Promise<GroupMembersResponse> {
  return api<GroupMembersResponse>(`/api/v1/groups/${encodeURIComponent(group)}/members`);
}

export function setGroupMember(group: string, user: string, rank: string): Promise<void> {
  return api(`/api/v1/groups/${encodeURIComponent(group)}/members`, {
    method: "PUT",
    body: JSON.stringify({ user, rank }),
  });
}

export function getRepoMembers(repo: string): Promise<RepoMembersResponse> {
  return api<RepoMembersResponse>(`/api/v1/repos/${encodeURIComponent(repo)}/members`);
}

export function setRepoMember(repo: string, user: string, rank: string): Promise<void> {
  return api(`/api/v1/repos/${encodeURIComponent(repo)}/members`, {
    method: "PUT",
    body: JSON.stringify({ user, rank }),
  });
}
