// BlameView.tsx — render parsed --line-porcelain blame. Each run of
// consecutive same-commit lines gets ONE single-line attribution on its
// first row (commit · subject · author · date) so every source line keeps
// the same height; a hairline separates the runs. Boundary commits wear
// git's own "^" prefix.
import type { ReactNode } from "react";
import type { BlameLine } from "../api/blame";
import { shortOid, whenStr } from "./ui";

export function BlameView({ lines }: { lines: BlameLine[] }): ReactNode {
  if (lines.length === 0) return <p className="muted pad">no blame output</p>;
  return (
    <table className="code-table blame">
      <tbody>
        {lines.map((l, i) => {
          const newGroup = i === 0 || lines[i - 1].oid !== l.oid;
          return (
            <tr key={i} className={newGroup && i > 0 ? "group-start" : ""}>
              <td className="blame-meta">
                {newGroup ? (
                  <div className="blame-line" title={`${l.author} <${l.authorMail}> · ${whenStr(l.authorTime)}\n${l.summary}`}>
                    <a className="blame-oid" href={`#commit-${l.oid}`} title={l.boundary ? "boundary commit" : undefined}>
                      {l.boundary ? "^" : ""}
                      {shortOid(l.oid)}
                    </a>
                    <span className="blame-subject">{l.summary}</span>
                    <span className="blame-author">{l.author}</span>
                    <span className="blame-when muted">{whenStr(l.authorTime).slice(0, 10)}</span>
                  </div>
                ) : null}
              </td>
              <td className="lineno">{i + 1}</td>
              <td className="codetext">{l.text || " "}</td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}
