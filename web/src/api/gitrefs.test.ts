import { describe, expect, it } from "vitest";
import { branchesOf, parseAdvertisement } from "./gitrefs";

// pkt helper: encode one pkt-line
function pkt(line: string): Buffer {
  const payload = Buffer.from(line, "utf8");
  const head = (payload.length + 4).toString(16).padStart(4, "0");
  return Buffer.concat([Buffer.from(head, "ascii"), payload]);
}

const adv = Buffer.concat([
  pkt("# service=git-upload-pack\n"),
  Buffer.from("0000", "ascii"),
  // HEAD carries the symref capability after NUL
  pkt("0000000000000000000000000000000000000000 HEAD\0symref=HEAD:refs/heads/main object-format=sha1 agent=git/2.47\n"),
  pkt("1111111111111111111111111111111111111111 refs/heads/feature\n"),
  pkt("2222222222222222222222222222222222222222 refs/heads/zebra\n"),
  pkt("3333333333333333333333333333333333333333 refs/heads/main\n"),
  pkt("4444444444444444444444444444444444444444 refs/tags/v1\n"),
  Buffer.from("0000", "ascii"),
]);

describe("parseAdvertisement", () => {
  it("reads refs, drops the service header, keeps capabilities", () => {
    const ad = parseAdvertisement(new Uint8Array(adv));
    expect(ad.refs.map((r) => r.name)).toEqual([
      "HEAD",
      "refs/heads/feature",
      "refs/heads/zebra",
      "refs/heads/main",
      "refs/tags/v1",
    ]);
    expect(ad.capabilities).toContain("symref=HEAD:refs/heads/main");
    expect(ad.defaultBranch).toBe("main");
  });

  it("distills branches with the default first", () => {
    const ad = parseAdvertisement(new Uint8Array(adv));
    const rows = branchesOf(ad);
    expect(rows[0]).toEqual({ name: "main", oid: "3333333333333333333333333333333333333333", isDefault: true });
    expect(rows.map((r) => r.name)).toEqual(["main", "feature", "zebra"]);
  });

  it("accepts 64-hex oids (sha256 repos)", () => {
    const oid = "a".repeat(64);
    const raw = Buffer.concat([pkt(`${oid} refs/heads/main\n`), Buffer.from("0000", "ascii")]);
    const ad = parseAdvertisement(new Uint8Array(raw));
    expect(ad.refs[0].oid).toBe(oid);
  });
});
