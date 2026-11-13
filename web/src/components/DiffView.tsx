// DiffView.tsx — render a parsed unified diff: per-file headers with +/-
// stats, hunk headers, and add/del/context rows.
import type { ReactNode } from "react";
import { diffStat, type DiffFile } from "../api/diff";

export function DiffView({ files }: { files: DiffFile[] }): ReactNode {
  if (files.length === 0) return <p className="muted pad">no changes</p>;
  return (
    <div className="diff-files">
      {files.map((f, i) => {
        const stat = diffStat([f]);
        const path = f.newPath || f.oldPath || "(unnamed)";
        return (
          <div key={i} className="diff-file">
            <div className="diff-head">
              <code>{path}</code>
              <span className="diffstat">
                <span className="add">+{stat.add}</span> <span className="del">−{stat.del}</span>
              </span>
            </div>
            <table className="code-table diff">
              <tbody>
                {f.rows.map((r, j) => (
                  <tr key={j} className={r.kind}>
                    <td className="lineno">{r.oldLine ?? ""}</td>
                    <td className="lineno">{r.newLine ?? ""}</td>
                    <td className={"codetext " + (r.kind === "add" ? "plus" : r.kind === "del" ? "minus" : "")}>
                      {r.kind === "add" ? "+ " : r.kind === "del" ? "− " : r.kind === "hunk" ? "" : "  "}
                      {r.text || " "}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        );
      })}
    </div>
  );
}
