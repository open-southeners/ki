import { useQuery } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useApi } from "@/lib/rpc-context";
import { ErrorState, Mono, RowsSkeleton, StatusBadge } from "./common";
import { tunnelBadge } from "./logic";

/** Cloudflare Tunnel connector state; the token is set with the CLI and never shown. */
export function TunnelCard() {
  const api = useApi();
  const q = useQuery({ queryKey: ["tunnel", "status"], queryFn: () => api.tunnel.status(), refetchInterval: 30_000 });

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-2 p-4 pb-2">
        <CardTitle className="label-mono text-[11px] font-medium">Tunnel</CardTitle>
        {q.data && <StatusBadge spec={tunnelBadge(q.data.state)} />}
      </CardHeader>
      <CardContent className="grid gap-3 px-4 pb-4">
        {q.isPending ? (
          <RowsSkeleton rows={2} className="p-0" />
        ) : q.isError ? (
          <ErrorState title="Tunnel status unavailable" error={q.error} onRetry={() => void q.refetch()} className="p-0" />
        ) : !q.data.enabled ? (
          <p className="text-xs text-muted-foreground">
            Cloudflare Tunnel is off. Set <Mono className="text-xs">tunnel.enabled: true</Mono> to reach this server
            without open ports.
          </p>
        ) : (
          <>
            {q.data.warnings.map((w) => (
              <Alert key={w}>
                <TriangleAlert className="size-4" />
                <AlertDescription>{w}</AlertDescription>
              </Alert>
            ))}
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm">
              <dt className="label-mono self-center">Ready</dt>
              <dd>
                <Mono>{q.data.ready ? "yes" : "no"}</Mono>
              </dd>
              <dt className="label-mono self-center">Connections</dt>
              <dd>
                <Mono>{q.data.connections}</Mono>
              </dd>
              {q.data.state === "unconfigured" && (
                <>
                  <dt className="label-mono self-center">Token</dt>
                  <dd className="text-xs text-muted-foreground">
                    Run <Mono className="text-xs">ki agent tunnel configure --token-stdin</Mono> on the server.
                  </dd>
                </>
              )}
              {q.data.last_error && (
                <>
                  <dt className="label-mono self-center text-destructive">Error</dt>
                  <dd className="break-words text-destructive">{q.data.last_error}</dd>
                </>
              )}
            </dl>
          </>
        )}
      </CardContent>
    </Card>
  );
}
