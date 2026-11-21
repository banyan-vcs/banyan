// readme.test.ts — the README contract: which root file counts, how
// README-relative references normalize (the "../"-escape and "./"
// collapse rules), and which srcs survive the asset whitelist.
import { describe, expect, it, vi } from "vitest";
import { fetchReadme, normalizeRepoPath, pickReadme, repoAssetURL } from "./readme";
import { api } from "./client";

vi.mock("./client", () => ({ api: vi.fn(), qs: (p: Record<string, string>) => "?" + new URLSearchParams(p as Record<string, string>).toString(), BASE_URL: "/" }));
const apiMock = vi.mocked(api);

describe("pickReadme", () => {
  it("prefers README.md over README.markdown over nothing, any casing", () => {
    expect(pickReadme(["src", "README.md"])).toBe("README.md");
    expect(pickReadme(["readme.md", "Readme.markdown"])).toBe("readme.md");
    expect(pickReadme(["docs", "README.markdown"])).toBe("README.markdown");
    expect(pickReadme(["src", "notes.txt"])).toBeNull();
  });
});

describe("normalizeRepoPath", () => {
  it("resolves against the document directory, collapses ./ and .., treats / as root", () => {
    expect(normalizeRepoPath("img/a.png", "")).toEqual({ segs: ["img", "a.png"], tail: "" });
    expect(normalizeRepoPath("./img/a.png", "")).toEqual({ segs: ["img", "a.png"], tail: "" });
    expect(normalizeRepoPath("b/c.png", "docs/guide")).toEqual({ segs: ["docs", "guide", "b", "c.png"], tail: "" });
    expect(normalizeRepoPath("../up.png", "docs/guide")).toEqual({ segs: ["docs", "up.png"], tail: "" });
    expect(normalizeRepoPath("/root.png", "docs")).toEqual({ segs: ["root.png"], tail: "" });
    expect(normalizeRepoPath("a b/c#d.png", "")).toEqual({ segs: ["a b", "c"], tail: "#d.png" });
    expect(normalizeRepoPath("a.png?w=2", "")).toEqual({ segs: ["a.png"], tail: "?w=2" });
  });
  it("refuses escapes past the repo root and empty results", () => {
    expect(normalizeRepoPath("../../x.png", "docs")).toBeNull();
    expect(normalizeRepoPath("../x.png", "")).toBeNull();
    expect(normalizeRepoPath("/", "")).toBeNull();
  });
});

describe("repoAssetURL", () => {
  it("maps repo paths onto the raw face with segment encoding", () => {
    expect(repoAssetURL("docs/a b.png", "", "demo", "main")).toBe("/api/v1/repos/demo/raw/docs/a%20b.png?rev=main");
    expect(repoAssetURL("/x.png", "", "demo", "")).toBe("/api/v1/repos/demo/raw/x.png");
  });
  it("passes http(s)/data:image/blob through and kills other schemes", () => {
    expect(repoAssetURL("https://x/y.png", "", "d", "main")).toBe("https://x/y.png");
    expect(repoAssetURL("data:image/png;base64,AAAA", "", "d", "main")).toBe("data:image/png;base64,AAAA");
    expect(repoAssetURL("javascript:alert(1)", "", "d", "main")).toBeNull();
    expect(repoAssetURL("vbscript:x", "", "d", "main")).toBeNull();
  });
});

describe("fetchReadme", () => {
  it("walks tree → README entry → object bytes, null when none", async () => {
    apiMock.mockReset();
    apiMock.mockResolvedValueOnce({
      entries: [
        { name: "src", type: "tree", mode: "040000", oid: "t1" },
        { name: "README.md", type: "blob", mode: "100644", oid: "r1" },
      ],
    } as never);
    apiMock.mockResolvedValueOnce(new TextEncoder().encode("# hello").buffer as never);
    expect(await fetchReadme("demo", "main")).toEqual({ name: "README.md", text: "# hello" });

    apiMock.mockResolvedValueOnce({ entries: [{ name: "src", type: "tree", mode: "040000", oid: "t1" }] } as never);
    expect(await fetchReadme("demo", "main")).toBeNull();
  });
});
