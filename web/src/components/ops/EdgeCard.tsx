import { useQuery } from "@tanstack/react-query";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { formatDateTime } from "@/lib/format";
import { useApi } from "@/lib/rpc-context";
import type { EdgeStatus } from "@/lib/types";
import { ErrorState, Mono, RowsSkeleton, StatusBadge } from "./common";
import type { BadgeSpec } from "./logic";

function edgeBadge(s: EdgeStatus["state"]): BadgeSpec {
  if (s === "running") return { tone: "success", label: "running" };
  if (s === "starting") return { tone: "warning", label: "starting" };
  if (s === "error") return { tone: "destructive", label: "error" };
  return { tone: "secondary", label: "disabled" };
}

export function EdgeCard() {
  const api = useApi();
  const q = useQuery({ queryKey: ["edge", "status"], queryFn: () => api.edge.status(), refetchInterval: 30_000 });

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-2 p-4 pb-2">
        <CardTitle className="label-mono text-[11px] font-medium">Edge</CardTitle>
        {q.data && <StatusBadge spec={edgeBadge(q.data.state)} />}
      </CardHeader>
      <CardContent className="px-4 pb-4">
        {q.isPending ? (
          <RowsSkeleton rows={2} className="p-0" />
        ) : q.isError ? (
          <ErrorState title="Edge status unavailable" error={q.error} onRetry={() => void q.refetch()} className="p-0" />
        ) : (
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm">
            <dt className="label-mono self-center">Ports</dt>
            <dd>
              <Mono>
                {q.data.http_port} / {q.data.https_port}
              </Mono>
            </dd>
            <dt className="label-mono self-center">Routes</dt>
            <dd>
              <Mono>{q.data.routes}</Mono>
            </dd>
            <dt className="label-mono self-center">Certs</dt>
            <dd>
              <Mono>{q.data.certificates.length}</Mono>
              {q.data.certificates.length === 0 && (
                <span className="ml-2 text-xs text-muted-foreground">
                  self-signed until {formatDateTime(q.data.self_signed.not_after)}
                </span>
              )}
            </dd>
            {q.data.last_error && (
              <>
                <dt className="label-mono self-center text-destructive">Error</dt>
                <dd className="break-words text-destructive">{q.data.last_error}</dd>
              </>
            )}
          </dl>
        )}
      </CardContent>
    </Card>
  );
}
