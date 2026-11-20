// CodeEditor.tsx — Monaco (the VS Code / OpenSumi editor core) as the
// code-display and online-edit surface. Direct ESM import with the editor
// worker bundled via Vite — no loader CDN, fully offline-capable; only the
// tokenizing basic languages are registered (no TS language services, no
// WASM). The component is code-split: it joins the bundle only when a file
// view switches to it or edit mode opens.
import * as monaco from "monaco-editor/editor/editor.api.js";
import EditorWorker from "monaco-editor/editor/editor.worker.js?worker";
import { useEffect, useRef, type ReactNode } from "react";

// The tokenizers (Monarch grammars, main-thread, tiny per language).
import "monaco-editor/languages/definitions/go/register.js";
import "monaco-editor/languages/definitions/python/register.js";
import "monaco-editor/languages/definitions/rust/register.js";
import "monaco-editor/languages/definitions/typescript/register.js";
import "monaco-editor/languages/definitions/javascript/register.js";
import "monaco-editor/languages/definitions/java/register.js";
import "monaco-editor/languages/definitions/cpp/register.js";
import "monaco-editor/languages/definitions/csharp/register.js";
import "monaco-editor/languages/definitions/php/register.js";
import "monaco-editor/languages/definitions/ruby/register.js";
import "monaco-editor/languages/definitions/kotlin/register.js";
import "monaco-editor/languages/definitions/swift/register.js";
import "monaco-editor/languages/definitions/sql/register.js";
import "monaco-editor/languages/definitions/yaml/register.js";
import "monaco-editor/languages/definitions/xml/register.js";
import "monaco-editor/languages/definitions/css/register.js";
import "monaco-editor/languages/definitions/html/register.js";
import "monaco-editor/languages/definitions/markdown/register.js";
import "monaco-editor/languages/definitions/ini/register.js";
import "monaco-editor/languages/definitions/shell/register.js";
import "monaco-editor/languages/definitions/dockerfile/register.js";
import "monaco-editor/languages/definitions/protobuf/register.js";

// One worker for the whole app (word-based suggestions, diff computation).
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(self as any).MonacoEnvironment = { getWorker: () => new EditorWorker() };

const LANG_BY_EXT: Record<string, string> = {
  go: "go", py: "python", rs: "rust", ts: "typescript", tsx: "typescript",
  js: "javascript", mjs: "javascript", jsx: "javascript", java: "java",
  c: "cpp", h: "cpp", cc: "cpp", cpp: "cpp", hpp: "cpp", cs: "csharp",
  php: "php", rb: "ruby", kt: "kotlin", kts: "kotlin", swift: "swift",
  sql: "sql", yaml: "yaml", yml: "yaml", xml: "xml", html: "html",
  htm: "html", css: "css", scss: "css", md: "markdown", markdown: "markdown",
  toml: "ini", ini: "ini", conf: "ini", sh: "shell", bash: "shell",
  proto: "protobuf",
};

export function monacoLangOf(path: string): string {
  const base = path.split("/").pop() || path;
  if (/^dockerfile(\..+)?$/i.test(base)) return "dockerfile";
  const dot = base.lastIndexOf(".");
  if (dot < 0) return "plaintext";
  return LANG_BY_EXT[base.slice(dot + 1).toLowerCase()] || "plaintext";
}

export function CodeEditor({
  path,
  value,
  readOnly,
  onChange,
  height = "60vh",
}: {
  path: string;
  value: string;
  readOnly: boolean;
  onChange?: (next: string) => void;
  height?: string;
}): ReactNode {
  const host = useRef<HTMLDivElement | null>(null);
  const editor = useRef<monaco.editor.IStandaloneCodeEditor | null>(null);

  useEffect(() => {
    if (!host.current) return;
    const ed = monaco.editor.create(host.current, {
      value,
      language: monacoLangOf(path),
      readOnly,
      minimap: { enabled: false },
      scrollBeyondLastLine: false,
      fontSize: 13,
      lineNumbersMinChars: 4,
      renderWhitespace: "selection",
      wordWrap: "off",
      theme: "vs",
      automaticLayout: true,
    });
    editor.current = ed;
    const sub = ed.onDidChangeModelContent(() => onChange?.(ed.getValue()));
    return () => {
      sub.dispose();
      ed.getModel()?.dispose();
      ed.dispose();
      editor.current = null;
    };
    // value/path changes remount deliberately (path switches file); the
    // edit-mode toggle is the only in-place transition and it recreates too.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, readOnly, value]);

  return <div ref={host} className="code-editor" style={{ height, border: "1px solid var(--line)" }} />;
}
