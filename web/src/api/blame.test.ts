import { describe, expect, it } from "vitest";
import { parseBlame } from "./blame";

const sample = [
  "0000000000000000000000000000000000000000 1 1 1",
  "author Alice",
  "author-mail <alice@example.com>",
  "author-time 1700000000",
  "summary first line",
  "\tthe first source line",
  "1111111111111111111111111111111111111111 2 2 1",
  "boundary",
  "author Bob",
  "author-mail <bob@example.com>",
  "author-time 1700000100",
  "summary second line",
  "\tthe second source line",
].join("\n");

describe("parseBlame", () => {
  it("yields one row per source line with attribution", () => {
    const lines = parseBlame(sample);
    expect(lines).toHaveLength(2);
    expect(lines[0]).toMatchObject({ author: "Alice", oid: "0".repeat(40), text: "the first source line", boundary: false });
    expect(lines[1]).toMatchObject({ author: "Bob", oid: "1".repeat(40), text: "the second source line", boundary: true });
  });

  it("handles empty input", () => {
    expect(parseBlame("")).toEqual([]);
  });
});
