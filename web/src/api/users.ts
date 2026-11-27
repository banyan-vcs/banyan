// users.ts — the account-avatar client (docs/design/merge-requests.md
// §review, the avatar spec): the profile page PUTs its own, the console
// reads anyone's by name. The value is a data:image/ or https:// URL (or
// empty — the name-initial default is a client-side render, not a server
// asset).
import { api } from "./client";

export interface UserAvatar {
  user: string;
  avatarUrl: string;
}

export const getUserAvatar = (name: string): Promise<UserAvatar> =>
  api<UserAvatar>(`/api/v1/users/${encodeURIComponent(name)}/avatar`);

export const putMyAvatar = (avatarUrl: string): Promise<UserAvatar> =>
  api<UserAvatar>("/api/v1/me/avatar", { method: "PUT", body: JSON.stringify({ avatarUrl }) });
