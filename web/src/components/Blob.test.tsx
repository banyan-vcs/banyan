import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { BlobView, looksBinary } from "./Blob";

describe("looksBinary", () => {
  it("flags NUL bytes", () => {
    expect(looksBinary(new Uint8Array([65, 0, 66]))).toBe(true);
  });
  it("passes plain text", () => {
    expect(looksBinary(new TextEncoder().encode("hello\nworld"))).toBe(false);
  });
});

describe("BlobView", () => {
  it("renders numbered lines for text", () => {
    const bytes = new TextEncoder().encode("alpha\nbeta");
    render(<BlobView name="notes.txt" bytes={bytes} size={bytes.length} />);
    expect(screen.getByText("alpha")).toBeTruthy();
    expect(screen.getByText("beta")).toBeTruthy();
    expect(screen.getByText("2")).toBeTruthy();
  });

  it("renders a binary notice for NUL bytes", () => {
    const bytes = new Uint8Array([1, 0, 1]);
    render(<BlobView name="blob.bin" bytes={bytes} size={3} />);
    expect(screen.getByText(/binary file/)).toBeTruthy();
  });
});
