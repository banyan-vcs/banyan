// FileIcon.tsx — a tiny glyph per entry kind (directories, files, links,
// submodules), text-styled to match the console's plain aesthetic.

const BY_EXT: Record<string, string> = {
  go: "go",
  ts: "ts",
  tsx: "ts",
  js: "js",
  mjs: "js",
  json: "{}",
  md: "md",
  yaml: "yml",
  yml: "yml",
  py: "py",
  rs: "rs",
  java: "jv",
  c: "c",
  h: "c",
  cpp: "c++",
  sql: "sql",
  sh: "sh",
  css: "css",
  html: "html",
  svg: "svg",
  png: "img",
  jpg: "img",
  jpeg: "img",
  gif: "img",
  txt: "txt",
  mod: "go",
  sum: "go",
};

export function FileIcon({ name, kind, mode }: { name: string; kind: "tree" | "blob" | "commit"; mode: string }): string {
  if (kind === "tree") return "/";
  if (mode === "160000") return "⊕";
  if (mode === "120000") return "→";
  const ext = name.includes(".") ? name.split(".").pop()! : "";
  return BY_EXT[ext] || "·";
}
