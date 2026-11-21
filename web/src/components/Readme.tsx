// Readme.tsx — the README panel of the repository home: the root's
// README rendered as Markdown (docs/web/web-layer.md), with the document's
// relative references mapped onto this console — images to the raw face
// (GET /repos/<repo>/raw/<path>, same-origin through the web tier's
// credential-injecting proxy), file links to the tree route. react-markdown
// renders React elements and strips raw HTML by construction; the URL
// whitelist lives in repoAssetURL (javascript: and friends die there).
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { fetchReadme, normalizeRepoPath, repoAssetURL } from "../api/readme";
import { Spinner } from "./ui";

export function ReadmeSection({ repo, rev, base }: { repo: string; rev: string; base: string }): ReactNode {
  const readme = useQuery({
    queryKey: ["readme", repo, rev],
    queryFn: () => fetchReadme(repo, rev),
    enabled: rev !== "",
    retry: false,
  });
  if (rev === "" || readme.isError) return null;
  if (readme.isPending) return <Spinner />;
  if (!readme.data) return null;
  return (
    <section className="readme-card">
      <div className="readme-bar">
        <span className="fname">{readme.data.name}</span>
        <span className="ref">{rev}</span>
      </div>
      <ReadmeBody text={readme.data.text} repo={repo} rev={rev} base={base} />
    </section>
  );
}

export function ReadmeBody({ text, repo, rev, base, dir = "" }: { text: string; repo: string; rev: string; base: string; dir?: string }): ReactNode {
  return (
    <div className="md-body">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        urlTransform={(u) => u} // the img/a overrides below are the whitelist
        components={{
          img: ({ src, alt, title }) => {
            if (typeof src !== "string") return null;
            const url = repoAssetURL(src, dir, repo, rev);
            return url === null ? <span className="muted">[blocked image]</span> : <img src={url} alt={alt ?? ""} title={title} />;
          },
          a: ({ href, children }) => {
            if (typeof href !== "string" || href === "") return <span>{children}</span>;
            if (/^(https?:|mailto:|tel:)/i.test(href)) {
              return /^https?:/i.test(href) ? (
                <a href={href} target="_blank" rel="noreferrer">
                  {children}
                </a>
              ) : (
                <a href={href}>{children}</a>
              );
            }
            if (href.startsWith("#")) return <a href={href}>{children}</a>;
            if (/^[a-z][a-z0-9+.-]*:/i.test(href)) return <span>{children}</span>;
            const norm = normalizeRepoPath(href.split("#")[0].split("?")[0], dir);
            if (!norm) return <span>{children}</span>;
            const to =
              `${base}/tree/${norm.segs.map(encodeURIComponent).join("/")}` +
              (rev ? `?ref=${encodeURIComponent(rev)}` : "") +
              (href.includes("#") ? `#${href.slice(href.indexOf("#") + 1)}` : "");
            return <Link to={to}>{children}</Link>;
          },
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
}
