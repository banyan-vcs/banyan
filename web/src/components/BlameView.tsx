// BlameView.tsx — render parsed --line-porcelain blame: author + date +
// commit per source line, consecutive same-commit groups shaded.
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
            <tr key={i} className={i % 2 === 0 ? "even" : "odd"}>
              <td className="blame-meta">
                {newGroup ? (
                  <>
                    <span className="blame-author" title={l.authorMail}>
                      {l.author}
                    </span>{" "}
                    <span className="muted">{whenStr(l.authorTime)}</span>{" "}
                    <a className="blame-oid" href={`#commit-${l.oid}`}>
                      {shortOid(l.oid)}
                    </a>
                    {l.boundary ? <span className="pill">boundary</span> : null}
                    <div className="blame-subject muted">{l.summary}</div>
                  </>
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
