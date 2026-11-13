// mode.tsx — the deployment's shape as /api/mode advertises it: the
// profile (standalone | gitlab), the configured login providers, and the
// capability list the SPA narrows by. In the gitlab profile GitLab owns
// project management and code browsing; banyan surfaces its own subtree
// features only.
import { useQuery } from "@tanstack/react-query";
import { createContext, useContext, type ReactNode } from "react";
import { BASE_URL } from "./api/client";

export interface ModeProvider {
  id: string;
  kind: string; // gitlab | github
  label: string;
}

export interface ModeInfo {
  profile: "standalone" | "gitlab" | string;
  sso: boolean;
  providers: ModeProvider[];
  capabilities: Record<string, boolean>;
}

const EMPTY: ModeInfo = {
  profile: "standalone",
  sso: false,
  providers: [],
  capabilities: {
    createRepos: true, code: true, history: true, branches: true,
    subtrees: true, settings: true, tokens: true, import: true,
  },
};

const ModeCtx = createContext<ModeInfo>(EMPTY);

export function ModeProvider({ children }: { children: ReactNode }): ReactNode {
  const { data } = useQuery({
    queryKey: ["mode"],
    queryFn: async (): Promise<ModeInfo> => {
      const resp = await fetch(`${BASE_URL}api/mode`, { credentials: "same-origin" });
      if (!resp.ok) throw new Error(`mode check failed: ${resp.status}`);
      return (await resp.json()) as ModeInfo;
    },
    retry: false,
    staleTime: Infinity,
  });
  return <ModeCtx.Provider value={data ?? EMPTY}>{children}</ModeCtx.Provider>;
}

export function useMode(): ModeInfo {
  return useContext(ModeCtx);
}

/** can reports whether the deployment exposes a capability. */
export function can(mode: ModeInfo, capability: string): boolean {
  return !!mode.capabilities[capability];
}
