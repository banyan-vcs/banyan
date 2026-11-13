import { describe, expect, it } from "vitest";
import { suggestName } from "./imports";

describe("suggestName", () => {
  it("derives the last segment from full URLs", () => {
    expect(suggestName("https://github.com/octocat/Hello-World")).toBe("Hello-World");
    expect(suggestName("https://gitlab.com/grp/sub/repo.git")).toBe("repo");
    expect(suggestName("http://gitlab.internal:8180/owner/repo.git/")).toBe("repo");
  });
  it("handles bare shapes and empties", () => {
    expect(suggestName("owner/repo.git")).toBe("repo");
    expect(suggestName("")).toBe("");
    expect(suggestName("https://gitlab.com/")).toBe("");
  });
});
