// sshkeys.ts — the user's SSH public keys: list your own, add one (the
// authorized_keys line; the gateway parses and answers the fingerprint),
// remove one by fingerprint (it rides a query parameter — the fingerprint
// is base64 payload and may contain '/').
import { api } from "./client";

export interface SSHKeyInfo {
  fingerprint: string;
  type: string;
  comment: string;
  addedAt: number;
}

export function getSSHKeys(): Promise<{ keys: SSHKeyInfo[] | null }> {
  return api<{ keys: SSHKeyInfo[] | null }>("/api/v1/auth/ssh-keys");
}

export function addSSHKey(key: string): Promise<{ fingerprint: string }> {
  return api<{ fingerprint: string }>("/api/v1/auth/ssh-keys", {
    method: "POST",
    body: JSON.stringify({ key }),
  });
}

export function deleteSSHKey(fingerprint: string): Promise<void> {
  return api(`/api/v1/auth/ssh-keys?fingerprint=${encodeURIComponent(fingerprint)}`, {
    method: "DELETE",
  });
}
