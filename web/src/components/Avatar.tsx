// Avatar.tsx — the avatar spec from the v2 visual design, one rule for the
// whole console: the default is the name's first character (中文首字 or a
// Latin initial keeping its case) on --surface-2 in ink — monochrome only,
// never per-user colors (an identicon rainbow would blow the color
// budget). A custom avatar (data: or https: URL from the account table)
// replaces it; a load failure falls back to the initial so a dead URL
// never leaves a hole. ring="ok" draws the status ring — approvals signal
// with the same green the merge box uses.
import { useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { getUserAvatar } from "../api/users";

// avatarInitial answers the default face: the first code point, case kept
// (a = alice stays lowercase — the mockup's explicit call).
export function avatarInitial(name: string): string {
  for (const ch of name) return ch;
  return "?";
}

export function Avatar({
  name,
  size = 20,
  ring,
  title,
}: {
  name: string;
  size?: 16 | 20 | 26 | 34;
  ring?: "ok";
  title?: string;
}): ReactNode {
  const [failed, setFailed] = useState(false);
  const av = useQuery({
    queryKey: ["avatar", name],
    queryFn: () => getUserAvatar(name),
    staleTime: 5 * 60 * 1000,
    retry: false,
  });
  const url = av.data?.avatarUrl || "";
  const cls = `avatar av-${size}` + (ring === "ok" ? " ring-ok" : "");
  if (url && !failed) {
    return <img className={cls} src={url} alt="" title={title || name} onError={() => setFailed(true)} />;
  }
  return (
    <span className={cls} data-avatar-initial={avatarInitial(name)} title={title || name} aria-hidden="true">
      {avatarInitial(name)}
    </span>
  );
}
