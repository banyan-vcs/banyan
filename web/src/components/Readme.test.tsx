// Readme.test.tsx — the rendered README: images and file links rewrite
// onto this console (raw face / tree route), external links open in a new
// tab, hostile schemes render inert, and raw HTML in the document never
// becomes elements.
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ReadmeBody } from "./Readme";

const body = (text: string) =>
  render(
    <MemoryRouter>
      <ReadmeBody text={text} repo="demo" rev="main" base="/r/demo" />
    </MemoryRouter>
  );

describe("ReadmeBody", () => {
  it("renders headings, code, and gfm tables", () => {
    body("# Title\n\n| a | b |\n|---|---|\n| 1 | 2 |");
    expect(screen.getByText("Title")).toBeTruthy();
    expect(screen.getByText("1")).toBeTruthy();
  });

  it("rewrites relative images onto the raw face with the ref pinned", () => {
    body("![logo](assets/logo.png)");
    const img = screen.getByAltText("logo") as HTMLImageElement;
    expect(img.getAttribute("src")).toBe("/api/v1/repos/demo/raw/assets/logo.png?rev=main");
  });

  it("passes external images through untouched", () => {
    body("![x](https://cdn.example.com/x.png)");
    const img = screen.getByAltText("x") as HTMLImageElement;
    expect(img.getAttribute("src")).toBe("https://cdn.example.com/x.png");
  });

  it("renders hostile image schemes inert instead of passing them on", () => {
    body("![evil](javascript:alert(1))");
    expect(screen.getByText("[blocked image]")).toBeTruthy();
  });

  it("rewrites relative file links onto the tree route, directories included", () => {
    body("[guide](docs/guide.md) and [src](/src)");
    expect(screen.getByText("guide").getAttribute("href")).toBe("/r/demo/tree/docs/guide.md?ref=main");
    expect(screen.getByText("src").getAttribute("href")).toBe("/r/demo/tree/src?ref=main");
  });

  it("opens external links in a new tab and keeps anchors", () => {
    body("[site](https://example.com) [top](#top)");
    const site = screen.getByText("site").closest("a");
    expect(site?.getAttribute("href")).toBe("https://example.com");
    expect(site?.getAttribute("target")).toBe("_blank");
    expect(screen.getByText("top").getAttribute("href")).toBe("#top");
  });

  it("never turns raw HTML in the document into elements", () => {
    body('<img src=x onerror="alert(1)">');
    expect(document.querySelector("img[src='x']")).toBeNull();
  });
});
