import { useQuery, useQueryClient } from "@tanstack/react-query";
import * as React from "react";
import { Navigate, Outlet, useLocation } from "react-router-dom";
import { getSession, logout as apiLogout, roleAtLeast, type SessionUser } from "./auth";
import type { Role } from "./types";

const SESSION_KEY = ["session"] as const;

interface AuthCtx {
  /** undefined while loading, null when signed out. */
  user: SessionUser | null | undefined;
  /** Replace the cached session (after login/setup, or when the RPC link reports 401). */
  setUser: (u: SessionUser | null) => void;
  signOut: () => Promise<void>;
}

const Ctx = React.createContext<AuthCtx | null>(null);

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: SESSION_KEY,
    queryFn: ({ signal }) => getSession(signal),
    staleTime: 60_000,
    retry: false,
  });
  const setUser = React.useCallback((u: SessionUser | null) => qc.setQueryData(SESSION_KEY, u), [qc]);
  const signOut = React.useCallback(async () => {
    try {
      await apiLogout();
    } finally {
      qc.clear();
      qc.setQueryData(SESSION_KEY, null);
    }
  }, [qc]);
  const value = React.useMemo<AuthCtx>(
    () => ({ user: q.isPending ? undefined : q.isError ? null : q.data, setUser, signOut }),
    [q.isPending, q.isError, q.data, setUser, signOut],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useAuth(): AuthCtx {
  const c = React.useContext(Ctx);
  if (!c) throw new Error("useAuth outside AuthProvider");
  return c;
}

/** The signed-in role (from /auth/session, refined by auth.whoami once connected). */
const RoleCtx = React.createContext<Role | undefined>(undefined);
export const RoleProvider = RoleCtx.Provider;

export function useRole(): Role | undefined {
  return React.useContext(RoleCtx);
}

/** True when the signed-in role is at least `need`. Use to hide or disable actions. */
export function useCan(need: Role): boolean {
  return roleAtLeast(useRole(), need);
}

export function FullScreenMessage({ children }: { children: React.ReactNode }) {
  return (
    <div className="grid min-h-screen place-items-center bg-background" role="status" aria-live="polite">
      <p className="label-mono animate-ki-pulse">{children}</p>
    </div>
  );
}

/** Route guard: redirects to /login (remembering where the user was going) without a session. */
export function RequireAuth() {
  const { user } = useAuth();
  const loc = useLocation();
  if (user === undefined) return <FullScreenMessage>Checking session</FullScreenMessage>;
  if (user === null) {
    const next = loc.pathname + loc.search;
    return <Navigate to={next && next !== "/" ? `/login?next=${encodeURIComponent(next)}` : "/login"} replace />;
  }
  return <Outlet />;
}

/** Route guard: sends users below `role` to the overview. Must sit inside the shell (needs RoleProvider). */
export function RequireRole({ role }: { role: Role }) {
  const have = useRole();
  if (have === undefined) return null;
  if (!roleAtLeast(have, role)) return <Navigate to="/" replace />;
  return <Outlet />;
}
