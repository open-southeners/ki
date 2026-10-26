import { useQuery } from "@tanstack/react-query";
import { Boxes, Globe, KeyRound, LayoutDashboard, Menu, ScrollText, Users, X, type LucideIcon } from "lucide-react";
import * as React from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { ConnectionIndicator } from "@/components/shell/ConnectionIndicator";
import { Wordmark } from "@/components/shell/Logo";
import { UserMenu } from "@/components/shell/UserMenu";
import { roleAtLeast } from "@/lib/auth";
import { RoleProvider, useAuth, useRole } from "@/lib/auth-context";
import { RpcProvider, useApi, useConnectionState, useRpc } from "@/lib/rpc-context";
import { cn } from "@/lib/utils";

interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
  end?: boolean;
}

const MAIN: NavItem[] = [
  { to: "/", label: "Overview", icon: LayoutDashboard, end: true },
  { to: "/projects", label: "Projects", icon: Boxes },
];

const ADMIN: NavItem[] = [
  { to: "/admin/users", label: "Users", icon: Users },
  { to: "/admin/tokens", label: "Tokens", icon: KeyRound },
  { to: "/admin/audit", label: "Audit", icon: ScrollText },
  { to: "/admin/edge", label: "Edge", icon: Globe },
];

/** Layout route for every signed-in page: owns the RPC connection, the role and the navigation. */
export function AppShell() {
  return (
    <RpcProvider>
      <RoleGate />
    </RpcProvider>
  );
}

/** Resolves the role (session first, then the agent's auth.whoami) and provides it to the pages. */
function RoleGate() {
  const { user } = useAuth();
  const api = useApi();
  const conn = useConnectionState();
  const who = useQuery({
    queryKey: ["whoami"],
    queryFn: () => api.auth.whoami(),
    enabled: conn === "open",
    staleTime: 30_000,
  });
  const role = who.data?.role ?? user?.role;
  return (
    <RoleProvider value={role}>
      <Frame />
    </RoleProvider>
  );
}

function Frame() {
  const role = useRole();
  const rpc = useRpc();
  const conn = useConnectionState();
  const [open, setOpen] = React.useState(false);
  const loc = useLocation();
  React.useEffect(() => setOpen(false), [loc.pathname]);

  const isAdmin = roleAtLeast(role, "admin");
  return (
    <div className="min-h-screen lg:grid lg:grid-cols-[15rem_1fr]">
      <a
        href="#main"
        className="sr-only z-50 rounded-md bg-primary px-3 py-2 text-primary-foreground focus:not-sr-only focus:fixed focus:left-3 focus:top-3"
      >
        Skip to content
      </a>

      <header className="flex items-center justify-between border-b bg-rail px-4 py-2.5 lg:hidden">
        <Wordmark />
        <Button
          variant="ghost"
          size="icon"
          aria-label={open ? "Close navigation" : "Open navigation"}
          aria-expanded={open}
          aria-controls="primary-nav"
          onClick={() => setOpen((v) => !v)}
        >
          {open ? <X /> : <Menu />}
        </Button>
      </header>

      <aside
        id="primary-nav"
        className={cn(
          "flex-col gap-6 border-r bg-rail px-3 py-4 lg:sticky lg:top-0 lg:flex lg:h-dvh lg:self-start lg:overflow-y-auto",
          open ? "flex border-b lg:border-b-0" : "hidden",
        )}
      >
        <Wordmark className="hidden px-2 lg:flex" />
        <nav aria-label="Primary" className="flex flex-1 flex-col gap-6 overflow-y-auto">
          <NavGroup items={MAIN} />
          {isAdmin && <NavGroup title="Admin" items={ADMIN} />}
        </nav>
        <div className="flex flex-col gap-3">
          <div className="px-2">
            <ConnectionIndicator />
            {rpc.hello && conn === "open" && (
              <p className="mt-1 font-mono text-[11px] text-muted-foreground">
                agent {rpc.hello.agent.version} · proto {rpc.hello.protocol}
              </p>
            )}
          </div>
          <UserMenu />
        </div>
      </aside>

      <main id="main" tabIndex={-1} className="min-w-0 px-4 py-6 sm:px-8 lg:py-8">
        <div className="mx-auto max-w-6xl animate-ki-in">
          <Outlet />
        </div>
      </main>
    </div>
  );
}

function NavGroup({ title, items }: { title?: string; items: NavItem[] }) {
  return (
    <div className="flex flex-col gap-0.5">
      {title && <p className="label-mono px-2 pb-1.5">{title}</p>}
      {items.map((it) => (
        <NavLink
          key={it.to}
          to={it.to}
          end={it.end}
          className={({ isActive }) =>
            cn(
              "group flex items-center gap-2.5 rounded-md border-l-2 px-2.5 py-1.5 text-sm font-medium transition-colors",
              isActive
                ? "border-primary bg-accent text-accent-foreground"
                : "border-transparent text-muted-foreground hover:bg-accent/60 hover:text-foreground",
            )
          }
        >
          <it.icon className="size-4 shrink-0" aria-hidden />
          {it.label}
        </NavLink>
      ))}
    </div>
  );
}
