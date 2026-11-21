// commitEdit.ts — the web's server-side assembly path (the thin CLI's
// publish route, docs/design/merge-requests.md §5): hash the blob with
// WebCrypto, upload its bytes, land one commit through POST /api/v1/commit
// CAS'd against the given base. Branch protections apply unchanged
// wherever it is used — the tree's edit button and a merge request's
// inline edit alike.
import { api } from "./client";

// The wire round-trip is whole-file; past this, edit via clone.
export const EDIT_CAP = 256 * 1024;

async function sha1Hex(bytes: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-1", bytes as unknown as ArrayBuffer);
  return Array.from(new Uint8Array(digest))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

function toBase64(bytes: Uint8Array): string {
  let out = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    out += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(out);
}

/** The git blob oid of a content string: sha1("blob <len>\0" + bytes). */
export async function blobOidHex(content: string): Promise<string> {
  const enc = new TextEncoder();
  const bytes = enc.encode(content);
  const header = enc.encode(`blob ${bytes.length}\0`);
  const both = new Uint8Array(header.length + bytes.length);
  both.set(header);
  both.set(bytes, header.length); // the offset is load-bearing: without it the bytes overwrite the header and every upload mismatches
  return sha1Hex(both);
}

/** Land one file's new content as a commit on branch (base = its head). */
export async function commitFileEdit(args: {
  repo: string;
  branch: string;
  base: string;
  path: string;
  content: string;
  message: string;
}): Promise<void> {
  const bytes = new TextEncoder().encode(args.content);
  const oid = await blobOidHex(args.content);
  await api("/api/v1/objects/content", {
    method: "POST",
    body: JSON.stringify({ repo: args.repo, objects: [{ oid, type: "blob", data: toBase64(bytes) }] }),
  });
  await api("/api/v1/commit", {
    method: "POST",
    body: JSON.stringify({
      repo: args.repo,
      branch: args.branch,
      base: args.base,
      message: args.message,
      changes: [{ path: args.path, blob: oid }],
    }),
  });
}
