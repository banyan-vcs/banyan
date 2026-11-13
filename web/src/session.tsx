// session.tsx — who is logged in (the demo is about per-user visibility).
import { useQuery } from "@tanstack/react-query";
import { createContext, useContext, type ReactNode } from "react";
import { BASE_URL } from "./api/client";

export interface SessionInfo {
  user: string;
  sso: boolean;
}

interface SessionState {
  info: SessionInfo | undefined;
  loading: boolean;
}

const SessionCtx = createContext<SessionState>({ info: undefined, loading: true });

export function SessionProvider({ children }: { children: ReactNode }) {
  const { data, isLoading } = useQuery({
    queryKey: ["session"],
    // raw fetch on purpose: 401 here means "not logged in", not "redirect"
    queryFn: async () => {
      const resp = await fetch(`${BASE_URL}api/session`, { credentials: "same-origin" });
      if (resp.status === 401) return null;
      if (!resp.ok) throw new Error(`session check failed: ${resp.status}`);
      return (await resp.json()) as SessionInfo;
    },
    retry: false,
    staleTime: 60_000,
  });
  return <SessionCtx.Provider value={{ info: data ?? undefined, loading: isLoading }}>{children}</SessionCtx.Provider>;
}

export function useSession(): SessionState {
  return useContext(SessionCtx);
}
