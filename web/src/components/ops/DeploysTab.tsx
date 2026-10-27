import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Rocket, Undo2 } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { isTerminal } from "@/lib/api";
import { useCan } from "@/lib/auth-context";
import { composeRef } from "@/lib/compose";
import { errorMessage, toastError } from "@/lib/errors";
import { formatRelative, shortSha } from "@/lib/format";
import { useApi } from "@/lib/rpc-context";
import type { Deploy } from "@/lib/types";
import { EmptyState, ErrorState, Mono, RowsSkeleton, StatusBadge } from "./common";
import { DeployDetailSheet } from "./DeployDetail";
import { deployErrorText, deployStatusBadge, previousGood, rollbackTargets } from "./logic";

const PAGE = 20;

export function DeploysTab({ slug, ready, compose = false }: { slug: string; ready: boolean; compose?: boolean }) {
  const api = useApi();
  const canOperate = useCan("operator");
  const q = useInfiniteQuery({
    queryKey: ["deploys", slug, "list"],
    initialPageParam: undefined as number | undefined,
    queryFn: ({ pageParam }) => api.deploys.list({ project: slug, limit: PAGE, before_id: pageParam }),
    getNextPageParam: (last) => (last.length === PAGE ? last[last.length - 1]?.id : undefined),
    // Follow runs in progress without the user reloading.
    refetchInterval: (query) => {
      const first = query.state.data?.pages[0];
      return first?.some((d) => !isTerminal(d.status)) ? 3000 : 20_000;
    },
  });

  const deploys = React.useMemo(() => q.data?.pages.flat() ?? [], [q.data]);
  const [selectedId, setSelectedId] = React.useState<number | null>(null);
  const selected = deploys.find((d) => d.id === selectedId) ?? null;
  const [nowOpen, setNowOpen] = React.useState(false);
  const [rollbackOpen, setRollbackOpen] = React.useState(false);

  const opened = (d: Deploy) => {
    setSelectedId(d.id);
  };

  return (
    <div className="grid gap-4">
      {canOperate && (
        <div className="flex flex-wrap gap-2">
          <Button onClick={() => setNowOpen(true)} disabled={!ready}>
            <Rocket /> Deploy now
          </Button>
          <Button variant="outline" onClick={() => setRollbackOpen(true)} disabled={!ready}>
            <Undo2 /> Rollback
          </Button>
          {!ready && <p className="self-center text-sm text-muted-foreground">The project is not ready to deploy yet.</p>}
        </div>
      )}

      <Card className="overflow-hidden">
        {q.isPending ? (
          <RowsSkeleton rows={4} />
        ) : q.isError ? (
          <ErrorState title="Could not load deploys" error={q.error} onRetry={() => void q.refetch()} />
        ) : deploys.length === 0 ? (
          <EmptyState title="No deploys yet">
            {canOperate ? "Use Deploy now, or push to the tracked branch." : "Nothing has been deployed for this project."}
          </EmptyState>
        ) : (
          <>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Deploy</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Ref</TableHead>
                  <TableHead>Commit</TableHead>
                  <TableHead>Source</TableHead>
                  <TableHead>When</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {deploys.map((d) => (
                  <TableRow key={d.id} className="cursor-pointer" onClick={() => opened(d)}>
                    <TableCell>
                      <button
                        type="button"
                        onClick={(e) => {
                          e.stopPropagation();
                          opened(d);
                        }}
                        className="rounded-sm font-mono font-medium text-primary underline-offset-4 hover:underline"
                        aria-label={`Open deploy ${d.id}`}
                      >
                        #{d.id}
                      </button>
                    </TableCell>
                    <TableCell>
                      <span className="inline-flex flex-wrap items-center gap-1.5">
                        <StatusBadge spec={deployStatusBadge(d.status)} />
                        {!isTerminal(d.status) && <Badge variant="outline">{d.stage}</Badge>}
                        {d.status === "failed" && <Badge variant="outline">{d.stage}</Badge>}
                        {d.rollback_of ? <Badge variant="secondary">↩ rollback of #{d.rollback_of}</Badge> : null}
                      </span>
                    </TableCell>
                    <TableCell>
                      <Mono className="break-all">{d.ref || "–"}</Mono>
                    </TableCell>
                    <TableCell>
                      <Mono>{shortSha(d.commit)}</Mono>
                    </TableCell>
                    <TableCell>
                      <Mono>{d.source}</Mono>
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-xs text-muted-foreground">{formatRelative(d.created_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            {q.hasNextPage && (
              <div className="border-t p-3 text-center">
                <Button size="sm" variant="outline" disabled={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()}>
                  {q.isFetchingNextPage ? "Loading…" : "Load older"}
                </Button>
              </div>
            )}
          </>
        )}
      </Card>

      <DeployDetailSheet deploy={selected} onClose={() => setSelectedId(null)} />
      <DeployNowDialog slug={slug} compose={compose} open={nowOpen} onOpenChange={setNowOpen} onStarted={opened} />
      <RollbackDialog slug={slug} deploys={deploys} open={rollbackOpen} onOpenChange={setRollbackOpen} onStarted={opened} />
    </div>
  );
}

function DeployNowDialog({
  slug,
  compose,
  open,
  onOpenChange,
  onStarted,
}: {
  slug: string;
  compose: boolean;
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onStarted: (d: Deploy) => void;
}) {
  const api = useApi();
  const qc = useQueryClient();
  const [ref, setRef] = React.useState("");
  const history = useQuery({
    queryKey: ["compose", slug, "history"],
    queryFn: () => api.projects.composeHistory({ slug, limit: 50 }),
    enabled: compose && open,
  });
  // Compose projects deploy a stored version; default to the newest.
  const versions = history.data ?? [];
  const chosen = ref || (versions[0] ? composeRef(versions[0].version) : "");
  const [error, setError] = React.useState<string | null>(null);
  const m = useMutation({
    mutationFn: () => api.deploys.trigger({ project: slug, ref: (compose ? chosen : ref.trim()) || undefined }),
    onMutate: () => setError(null),
    onSuccess: async (d) => {
      toast.success(`Deploy #${d.id} queued`);
      await qc.invalidateQueries({ queryKey: ["deploys", slug] });
      onOpenChange(false);
      setRef("");
      onStarted(d);
    },
    onError: (e) => {
      const friendly = deployErrorText(e);
      if (friendly) setError(friendly);
      toastError("Could not start the deploy", friendly ?? e);
    },
  });

  return (
    <Dialog open={open} onOpenChange={(o) => !m.isPending && onOpenChange(o)}>
      <DialogContent>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            m.mutate();
          }}
        >
          <DialogHeader>
            <DialogTitle>Deploy now</DialogTitle>
            <DialogDescription>
              {compose
                ? "Builds and starts the project from a stored compose version."
                : "Builds and starts the project from its tracked branch, or from a specific branch, tag or commit."}
            </DialogDescription>
          </DialogHeader>
          {compose ? (
            <div className="grid gap-1.5">
              <Label htmlFor="deploy-version">Compose version</Label>
              <Select value={chosen} onValueChange={setRef}>
                <SelectTrigger id="deploy-version">
                  <SelectValue placeholder={history.isPending ? "Loading…" : "No versions"} />
                </SelectTrigger>
                <SelectContent>
                  {versions.map((v) => (
                    <SelectItem key={v.version} value={composeRef(v.version)}>
                      v{v.version} · {formatRelative(v.created_at)}
                      {v.note ? ` · ${v.note}` : ""}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {history.isError && (
                <p role="alert" className="text-sm text-destructive">
                  {errorMessage(history.error)}
                </p>
              )}
              {error && (
                <p role="alert" className="text-sm text-destructive">
                  {error}
                </p>
              )}
            </div>
          ) : (
          <div className="grid gap-1.5">
            <Label htmlFor="deploy-ref">Ref (optional)</Label>
            <Input
              id="deploy-ref"
              className="font-mono"
              placeholder="tracked branch"
              value={ref}
              onChange={(e) => setRef(e.target.value)}
              autoComplete="off"
              spellCheck={false}
              aria-invalid={error !== null}
              aria-describedby={error ? "deploy-ref-err" : undefined}
            />
            {error && (
              <p id="deploy-ref-err" role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
          </div>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={m.isPending}>
              Cancel
            </Button>
            <Button type="submit" disabled={m.isPending || (compose && !chosen)}>
              {m.isPending ? "Starting…" : "Deploy"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

const PREVIOUS = "previous";

function RollbackDialog({
  slug,
  deploys,
  open,
  onOpenChange,
  onStarted,
}: {
  slug: string;
  deploys: Deploy[];
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onStarted: (d: Deploy) => void;
}) {
  const api = useApi();
  const qc = useQueryClient();
  const [target, setTarget] = React.useState(PREVIOUS);
  const [error, setError] = React.useState<string | null>(null);
  const targets = React.useMemo(() => rollbackTargets(deploys), [deploys]);
  const prev = previousGood(deploys);

  const m = useMutation({
    mutationFn: () => api.deploys.rollback({ project: slug, deploy: target === PREVIOUS ? undefined : Number(target) }),
    onMutate: () => setError(null),
    onSuccess: async (d) => {
      toast.success(`Rollback queued as #${d.id}`);
      await qc.invalidateQueries({ queryKey: ["deploys", slug] });
      onOpenChange(false);
      setTarget(PREVIOUS);
      onStarted(d);
    },
    onError: (e) => {
      const friendly = deployErrorText(e);
      if (friendly) setError(friendly);
      toastError("Could not start the rollback", friendly ?? e);
    },
  });

  return (
    <Dialog open={open} onOpenChange={(o) => !m.isPending && onOpenChange(o)}>
      <DialogContent>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            m.mutate();
          }}
        >
          <DialogHeader>
            <DialogTitle>Rollback</DialogTitle>
            <DialogDescription>
              Re-deploys an earlier successful deploy at its exact commit through the normal pipeline. The default is the
              deploy before the newest successful one.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-1.5">
            <Label htmlFor="rollback-target">Target</Label>
            <Select value={target} onValueChange={setTarget}>
              <SelectTrigger id="rollback-target" aria-describedby={error ? "rollback-err" : undefined}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={PREVIOUS}>
                  Previous good{prev ? ` (#${prev.id}, ${shortSha(prev.commit)})` : ""}
                </SelectItem>
                {targets.map((d) => (
                  <SelectItem key={d.id} value={String(d.id)}>
                    #{d.id} · {shortSha(d.commit)} · {d.ref || "–"} · {formatRelative(d.finished_at ?? d.created_at)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {targets.length < 2 && (
              <p className="text-sm text-muted-foreground">There is no earlier successful deploy to roll back to yet.</p>
            )}
            {error && (
              <p id="rollback-err" role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={m.isPending}>
              Cancel
            </Button>
            <Button type="submit" disabled={m.isPending}>
              {m.isPending ? "Starting…" : "Roll back"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
