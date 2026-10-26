import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronRight, ExternalLink, Play, RotateCw, ScrollText, Square } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { subscribeContainerStats } from "@/lib/api";
import { useCan } from "@/lib/auth-context";
import { toastError } from "@/lib/errors";
import { formatBytes, formatPercent, formatRate } from "@/lib/format";
import { useApi, useSubscription } from "@/lib/rpc-context";
import type { ContainerSample, ProjectService } from "@/lib/types";
import { cn } from "@/lib/utils";
import { EmptyState, ErrorState, Mono, RowsSkeleton, StatusBadge } from "./common";
import { ContainerLogsSheet, type LogTarget } from "./ContainerLogsSheet";
import { containerHealthBadge, containersRunning, exposureUrls, serviceStateBadge } from "./logic";

type Action = "start" | "stop" | "restart";

const ACTION_DONE: Record<Action, string> = { start: "started", stop: "stopped", restart: "restarted" };

/** Samples are keyed by container id; the runtime may report a short or full id. */
function sampleFor(samples: ContainerSample[], id: string): ContainerSample | undefined {
  return samples.find((s) => s.id === id || s.id.startsWith(id) || id.startsWith(s.id));
}

export function ServicesTab({ slug }: { slug: string }) {
  const api = useApi();
  const qc = useQueryClient();
  const canOperate = useCan("operator");
  const key = React.useMemo(() => ["services", slug] as const, [slug]);
  const q = useQuery({
    queryKey: key,
    queryFn: async () => (await api.projects.services({ slug })).services,
    refetchInterval: 10_000,
  });

  const [samples, setSamples] = React.useState<ContainerSample[]>([]);
  const [statsAvailable, setStatsAvailable] = React.useState(true);
  useSubscription(
    (rpc) =>
      subscribeContainerStats(
        rpc,
        { project: slug, interval_ms: 2000 },
        {
          onData: (u) => {
            setSamples(u.containers);
            setStatsAvailable(u.available);
          },
        },
      ),
    [slug],
  );

  const [expanded, setExpanded] = React.useState<Set<string>>(new Set());
  const [logs, setLogs] = React.useState<LogTarget | null>(null);
  const [confirmStop, setConfirmStop] = React.useState<string | null>(null);

  const act = useMutation({
    mutationFn: ({ service, action }: { service: string; action: Action }) => {
      const p = { slug, service };
      return action === "start"
        ? api.projects.serviceStart(p)
        : action === "stop"
          ? api.projects.serviceStop(p)
          : api.projects.serviceRestart(p);
    },
    onSuccess: (updated, { service, action }) => {
      toast.success(`${service} ${ACTION_DONE[action]}`);
      qc.setQueryData<ProjectService[]>(key, (old) => old?.map((s) => (s.name === updated.name ? updated : s)));
      void qc.invalidateQueries({ queryKey: key });
    },
    onError: (e, { service, action }) => toastError(`Could not ${action} ${service}`, e),
  });
  const busy = act.isPending ? act.variables?.service : undefined;

  if (q.isPending) return <Card><RowsSkeleton /></Card>;
  if (q.isError)
    return (
      <Card>
        <ErrorState title="Could not load services" error={q.error} onRetry={() => void q.refetch()} />
      </Card>
    );
  if (q.data.length === 0)
    return (
      <Card>
        <EmptyState title="No services">Deploy the project and its compose services will appear here.</EmptyState>
      </Card>
    );

  const toggle = (name: string) =>
    setExpanded((prev) => {
      const n = new Set(prev);
      if (n.has(name)) n.delete(name);
      else n.add(name);
      return n;
    });

  return (
    <>
      <Card className="overflow-hidden">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>Service</TableHead>
              <TableHead>State</TableHead>
              <TableHead>Containers</TableHead>
              <TableHead>Exposure</TableHead>
              {canOperate && <TableHead className="text-right">Actions</TableHead>}
            </TableRow>
          </TableHeader>
          <TableBody>
            {q.data.map((s) => {
              const open = expanded.has(s.name);
              const c = containersRunning(s);
              const urls = exposureUrls(s);
              const working = busy === s.name;
              return (
                <React.Fragment key={s.name}>
                  <TableRow>
                    <TableCell>
                      <button
                        type="button"
                        aria-expanded={open}
                        aria-controls={`svc-${s.name}`}
                        onClick={() => toggle(s.name)}
                        disabled={s.containers.length === 0}
                        className="inline-flex items-center gap-1.5 rounded-sm text-left disabled:opacity-60"
                      >
                        <ChevronRight className={cn("size-4 shrink-0 transition-transform", open && "rotate-90")} />
                        <span>
                          <span className="block font-mono font-medium">{s.name}</span>
                          {s.image && <span className="block max-w-[16rem] truncate font-mono text-[11px] text-muted-foreground">{s.image}</span>}
                        </span>
                      </button>
                    </TableCell>
                    <TableCell>
                      <StatusBadge spec={serviceStateBadge(s.state)} />
                    </TableCell>
                    <TableCell>
                      <Mono>
                        {c.running}/{c.total}
                      </Mono>
                    </TableCell>
                    <TableCell>
                      {urls.length === 0 ? (
                        <span className="text-muted-foreground">–</span>
                      ) : (
                        <ul className="grid gap-0.5">
                          {urls.map((u) => (
                            <li key={u}>
                              <a
                                href={u}
                                target="_blank"
                                rel="noreferrer noopener"
                                className="inline-flex items-center gap-1 font-mono text-[13px] text-primary underline-offset-4 hover:underline"
                              >
                                {u.replace(/^https:\/\//, "")}
                                <ExternalLink className="size-3" aria-hidden />
                              </a>
                            </li>
                          ))}
                        </ul>
                      )}
                    </TableCell>
                    {canOperate && (
                      <TableCell className="text-right">
                        <div className="inline-flex gap-1.5">
                          <Button
                            size="sm"
                            variant="outline"
                            disabled={working || s.state === "running"}
                            onClick={() => act.mutate({ service: s.name, action: "start" })}
                            aria-label={`Start ${s.name}`}
                          >
                            <Play /> <span className="hidden sm:inline">Start</span>
                          </Button>
                          <Button
                            size="sm"
                            variant="outline"
                            disabled={working || s.containers.length === 0}
                            onClick={() => act.mutate({ service: s.name, action: "restart" })}
                            aria-label={`Restart ${s.name}`}
                          >
                            <RotateCw className={cn(working && "animate-spin")} /> <span className="hidden sm:inline">Restart</span>
                          </Button>
                          <Button
                            size="sm"
                            variant="outline"
                            disabled={working || s.state === "stopped" || s.containers.length === 0}
                            onClick={() => setConfirmStop(s.name)}
                            aria-label={`Stop ${s.name}`}
                          >
                            <Square /> <span className="hidden sm:inline">Stop</span>
                          </Button>
                        </div>
                      </TableCell>
                    )}
                  </TableRow>
                  {open && (
                    <TableRow id={`svc-${s.name}`} className="bg-muted/30 hover:bg-muted/30">
                      <TableCell colSpan={canOperate ? 5 : 4} className="p-0">
                        <ContainerRows service={s} samples={samples} statsAvailable={statsAvailable} onLogs={setLogs} />
                      </TableCell>
                    </TableRow>
                  )}
                </React.Fragment>
              );
            })}
          </TableBody>
        </Table>
      </Card>

      <ContainerLogsSheet target={logs} onClose={() => setLogs(null)} />

      <AlertDialog open={confirmStop !== null} onOpenChange={(o) => !o && setConfirmStop(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Stop {confirmStop}?</AlertDialogTitle>
            <AlertDialogDescription>
              Every container of this service is stopped (10 s grace period). It stays stopped until you start it or the
              next deploy recreates it.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (confirmStop) act.mutate({ service: confirmStop, action: "stop" });
                setConfirmStop(null);
              }}
            >
              Stop service
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

function ContainerRows({
  service,
  samples,
  statsAvailable,
  onLogs,
}: {
  service: ProjectService;
  samples: ContainerSample[];
  statsAvailable: boolean;
  onLogs: (t: LogTarget) => void;
}) {
  return (
    <div className="px-5 py-3">
      {!statsAvailable && <p className="mb-2 text-xs text-muted-foreground">Live container stats are unavailable.</p>}
      <ul className="grid gap-2">
        {service.containers.map((c) => {
          const st = sampleFor(samples, c.id);
          return (
            <li key={c.id} className="grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[minmax(0,1.4fr)_auto_minmax(0,2fr)_auto] sm:items-center">
              <div className="min-w-0">
                <p className="truncate font-mono text-[13px]">{c.name}</p>
                <p className="truncate text-xs text-muted-foreground">{c.status}</p>
              </div>
              <div className="flex items-center gap-2">
                <StatusBadge spec={containerHealthBadge(c.health, c.state)} />
                {c.restart_count > 0 && <Mono className="text-xs text-warning">↻{c.restart_count}</Mono>}
              </div>
              <p className="font-mono text-[11px] text-muted-foreground">
                {st ? (
                  <>
                    cpu {formatPercent(st.cpu_percent)} · mem {formatBytes(st.memory.usage)}
                    {st.memory.percent !== null ? ` (${formatPercent(st.memory.percent, 0)})` : ""} · net ↓
                    {formatRate(st.network.rx_bytes_per_s)} ↑{formatRate(st.network.tx_bytes_per_s)}
                  </>
                ) : (
                  "no live stats"
                )}
              </p>
              <Button size="sm" variant="ghost" onClick={() => onLogs({ id: c.id, name: c.name })} aria-label={`Logs of ${c.name}`}>
                <ScrollText /> Logs
              </Button>
            </li>
          );
        })}
      </ul>
    </div>
  );
}
