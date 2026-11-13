// gitrefs.ts — parse the smart-HTTP v1 ref advertisement in the browser.
// The SAME filtered advertisement a git client sees is what makes branch
// visibility visible in the UI (personal branches of other users simply are
// not there). OID length follows the payload, so sha256 repos parse fine;
// the symref capability names the default branch.

import { BASE_URL, onUnauthorized } from "./client";
import type { RefEntry } from "./types";

export interface RefAdvertisement {
  refs: RefEntry[];
  capabilities: string;
  defaultBranch: string;
}

export async function listRefs(repo: string): Promise<RefAdvertisement> {
  const resp = await fetch(`${BASE_URL}r/${encodeURIComponent(repo)}.git/info/refs?service=git-upload-pack`, {
    credentials: "same-origin",
  });
  if (resp.status === 401) {
    onUnauthorized();
    throw new Error("not logged in");
  }
  if (!resp.ok) throw new Error(`${resp.status}: ${await resp.text().catch(() => resp.statusText)}`);
  return parseAdvertisement(new Uint8Array(await resp.arrayBuffer()));
}

export function parseAdvertisement(raw: Uint8Array): RefAdvertisement {
  const refs: RefEntry[] = [];
  let capabilities = "";
  const dec = new TextDecoder();
  let pos = 0;
  while (pos + 4 <= raw.length) {
    const n = parseInt(dec.decode(raw.subarray(pos, pos + 4)), 16);
    if (!n) {
      pos += 4;
      continue; // flush-pkt
    }
    if (n < 4 || pos + n > raw.length) break;
    let line = dec.decode(raw.subarray(pos + 4, pos + n));
    pos += n;
    line = line.replace(/\n$/, "");
    if (line.startsWith("#")) continue; // service header
    const nul = line.indexOf("\0");
    if (nul >= 0) {
      capabilities = line.slice(nul + 1);
      line = line.slice(0, nul);
    }
    const sp = line.indexOf(" ");
    if (sp < 0) continue;
    const oid = line.slice(0, sp);
    const name = line.slice(sp + 1);
    if (!/^[0-9a-f]{40,64}$/.test(oid) || !name) continue;
    refs.push({ oid, name });
  }
  const m = /symref=HEAD:refs\/heads\/([^\s]+)/.exec(capabilities);
  return { refs, capabilities, defaultBranch: m ? m[1] : "" };
}

// BranchRow is one branch of the advertisement, distilled.
export interface BranchRow {
  name: string;
  oid: string;
  isDefault: boolean;
}

export function branchesOf(ad: RefAdvertisement): BranchRow[] {
  const rows: BranchRow[] = [];
  for (const r of ad.refs) {
    if (!r.name.startsWith("refs/heads/")) continue;
    const name = r.name.slice("refs/heads/".length);
    rows.push({ name, oid: r.oid, isDefault: name === ad.defaultBranch });
  }
  rows.sort((a, b) => (a.isDefault === b.isDefault ? a.name.localeCompare(b.name) : a.isDefault ? -1 : 1));
  return rows;
}
