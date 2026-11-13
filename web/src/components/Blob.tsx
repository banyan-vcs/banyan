// Blob.tsx — render file content at a path: line numbers, syntax
// highlighting for known text types, binary detection, a preview cap and
// image previews. Lines are highlighted individually (ignoreIllegals) —
// multi-line constructs lose color but never break rendering, and a
// highlighting error on any single line falls back to plain text.
import hljs from "highlight.js/lib/common";
import { useMemo, type ReactNode } from "react";
import { sizeStr } from "./ui";

function safeHighlight(line: string, lang: string): string | null {
  try {
    return hljs.highlight(line, { language: lang, ignoreIllegals: true }).value;
  } catch {
    return null; // plain text fallback — never break the tree over one line
  }
}

const BLOB_PREVIEW = 64 * 1024;

const LANG_BY_EXT: Record<string, string> = {
  go: "go",
  ts: "typescript",
  tsx: "typescript",
  js: "javascript",
  mjs: "javascript",
  json: "json",
  md: "markdown",
  yaml: "yaml",
  yml: "yaml",
  py: "python",
  rs: "rust",
  java: "java",
  c: "c",
  h: "c",
  cpp: "cpp",
  hpp: "cpp",
  cs: "csharp",
  php: "php",
  rb: "ruby",
  sh: "bash",
  bash: "bash",
  css: "css",
  xml: "xml",
  html: "xml",
  sql: "sql",
  toml: "ini",
  ini: "ini",
};

const IMAGE_EXT = new Set(["png", "jpg", "jpeg", "gif", "svg", "webp", "ico"]);

export function looksBinary(bytes: Uint8Array): boolean {
  const n = Math.min(bytes.length, 8192);
  for (let i = 0; i < n; i++) {
    if (bytes[i] === 0) return true;
  }
  return false;
}

function base64Of(bytes: Uint8Array): string {
  let out = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    out += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(out);
}

export function BlobView({ name, bytes, size }: { name: string; bytes: Uint8Array; size: number }): ReactNode {
  const ext = name.includes(".") ? name.split(".").pop()! : "";

  const view = useMemo(() => {
    if (looksBinary(bytes)) return { kind: "binary" as const };
    if (IMAGE_EXT.has(ext) && size <= 2 * 1024 * 1024) {
      return { kind: "image" as const, src: `data:image/${ext === "svg" ? "svg+xml" : ext};base64,${base64Of(bytes)}` };
    }
    const truncated = bytes.length > BLOB_PREVIEW;
    const text = new TextDecoder("utf-8", { fatal: false }).decode(bytes.subarray(0, BLOB_PREVIEW));
    const lang = LANG_BY_EXT[ext];
    return { kind: "text" as const, text, lang: lang && hljs.getLanguage(lang) ? lang : null, truncated };
  }, [bytes, ext, size]);

  if (view.kind === "binary") {
    return (
      <div className="blob-box">
        <p className="muted pad">binary file · {sizeStr(size)}</p>
      </div>
    );
  }
  if (view.kind === "image") {
    return (
      <div className="blob-box">
        <img className="blob-image" src={view.src} alt={name} />
      </div>
    );
  }
  const lines = view.text.split("\n");
  if (lines.length > 0 && lines[lines.length - 1] === "") lines.pop();
  return (
    <div className="blob-box">
      {view.truncated && (
        <p className="warn-line">preview truncated at {sizeStr(BLOB_PREVIEW)} of {sizeStr(size)} — clone the subtree for the full file</p>
      )}
      <table className="code-table">
        <tbody>
          {lines.map((line, i) => (
            <tr key={i}>
              <td className="lineno">{i + 1}</td>
              <td className="codetext">
                {(() => {
                  const html = view.lang ? safeHighlight(line, view.lang) : null;
                  return html !== null ? (
                    <span dangerouslySetInnerHTML={{ __html: html }} />
                  ) : (
                    line || " "
                  );
                })()}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
