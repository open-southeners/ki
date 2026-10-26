import { useQueryClient } from "@tanstack/react-query";
import * as React from "react";
import { toast } from "sonner";
import { createApi, type Api } from "./api";
import { probeSession } from "./auth";
import { useAuth } from "./auth-context";
import { RpcClient, type ConnState, type Subscription } from "./rpc";

interface RpcCtx {
  rpc: RpcClient;
  api: Api;
}

const Ctx = React.createContext<RpcCtx | null>(null);

/** Owns the single WebSocket connection of a signed-in session. */
export function RpcProvider({ children }: { children: React.ReactNode }) {
  const { setUser } = useAuth();
  const qc = useQueryClient();
  const value = React.useMemo<RpcCtx>(() => {
    const rpc = new RpcClient({ probe: probeSession, client: { name: "ki-panel", version: "1" } });
    return { rpc, api: createApi(rpc) };
  }, []);

  React.useEffect(() => {
    const { rpc } = value;
    rpc.connect();
    let wasDown = false;
    const off = rpc.onStateChange(() => {
      const s = rpc.state;
      if (s === "unauthorized") {
        setUser(null); // the route guard sends the user to /login
      } else if (s === "reconnecting") {
        wasDown = true;
      } else if (s === "open" && wasDown) {
        wasDown = false;
        toast.success("Connection restored", { id: "conn" });
        void qc.invalidateQueries();
      }
    });
    return () => {
      off();
      rpc.close();
    };
  }, [value, setUser, qc]);

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

function useCtx(): RpcCtx {
  const c = React.useContext(Ctx);
  if (!c) throw new Error("RPC hooks need RpcProvider");
  return c;
}

export function useRpc(): RpcClient {
  return useCtx().rpc;
}

export function useApi(): Api {
  return useCtx().api;
}

/** Connection state of the shared client (re-renders on change). */
export function useConnectionState(): ConnState {
  const { rpc } = useCtx();
  return React.useSyncExternalStore(rpc.onStateChange, rpc.getState, rpc.getState);
}

/**
 * Starts a subscription for the lifetime of the component (the client holds it
 * until the link is open and re-subscribes after reconnects). `deps` as for
 * useEffect; changing them restarts the subscription.
 */
export function useSubscription(start: (rpc: RpcClient) => Subscription, deps: React.DependencyList): void {
  const rpc = useRpc();
  React.useEffect(() => {
    const s = start(rpc);
    return () => s.unsubscribe();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rpc, ...deps]);
}
