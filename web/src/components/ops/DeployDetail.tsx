import { useQueryClient } from "@tanstack/react-query";
import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { isTerminal, subscribeDeploy } from "@/lib/api";
import { formatDateTime, shortSha } from "@/lib/format";
import { useSubscription } from "@/lib/rpc-context";
import type { SubscriptionState } from "@/lib/rpc";
import type { Deploy, DeployUpdate } from "@/lib/types";
import { cn } from "@/lib/utils";
import { Mono, StatusBadge } from "./common";
import {
  deployStatusBadge,
  emptyDeployLog,
  failureReasonText,
  markDeployLogGap,
  reduceDeployLog,
  type DeployLogState,
} from "./logic";

export function DeployDetailSheet({ deploy, onClose }: { deploy: Deploy | null; onClose: () => void }) {
  return (
    <Sheet open={deploy !== null} onOpenChange={(o) => !o && onClose()}>
      <SheetContent aria-describedby="deploy-desc" className="sm:max-w-3xl">
        {deploy && <DeployBody key={deploy.id} initial={deploy} />}
      </SheetContent>
    </Sheet>
  );
}

type Ev = { kind: "update"; u: DeployUpdate } | { kind: "gap" };

function DeployBody({ initial }: { initial: Deploy }) {
  const qc = useQueryClient();
  const [log, setLog] = React.useState<DeployLogState>({ ...emptyDeployLog, deploy: initial });
  const [conn, setConn] = React.useState<SubscriptionState>("pending");
  const [follow, setFollow] = React.useState(true);
  const queue = React.useRef<Ev[]>([]);
  const timer = React.useRef<ReturnType<typeof setTimeout> | null>(null);
  const box = React.useRef<HTMLDivElement>(null);

  const flush = React.useCallback(() => {
    timer.current = null;
    const evs = queue.current;
    queue.current = [];
    if (evs.length === 0) return;
    setLog((s) => evs.reduce((acc, e) => (e.kind === "gap" ? markDeployLogGap(acc) : reduceDeployLog(acc, e.u)), s));
  }, []);

  const enqueue = React.useCallback(
    (e: Ev) => {
      queue.current.push(e);
      timer.current ??= setTimeout(flush, 80);
    },
    [flush],
  );

  React.useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current);
    },
    [],
  );

  useSubscription(
    (rpc) =>
      subscribeDeploy(rpc, initial.id, {
        onData: (u) => {
          enqueue({ kind: "update", u });
          if (u.type === "status" && isTerminal(u.deploy.status)) {
            void qc.invalidateQueries({ queryKey: ["deploys", initial.project] });
            void qc.invalidateQueries({ queryKey: ["services", initial.project] });
            void qc.invalidateQueries({ queryKey: ["projects"] });
          }
        },
        onGap: () => enqueue({ kind: "gap" }),
        onState: setConn,
      }),
    [initial.id],
  );

  React.useEffect(() => {
    const el = box.current;
    if (follow && el) el.scrollTop = el.scrollHeight;
  }, [log.lines, follow]);

  const d = log.deploy ?? initial;
  const live = !isTerminal(d.status);

  return (
    <>
      <SheetHeader>
        <SheetTitle className="flex flex-wrap items-center gap-2">
          <span>Deploy</span> <Mono className="text-base">#{d.id}</Mono>
          <StatusBadge spec={deployStatusBadge(d.status)} />
          {live && <Badge variant="outline">{d.stage}</Badge>}
        </SheetTitle>
        <SheetDescription id="deploy-desc" asChild>
          <div className="flex flex-wrap gap-x-4 gap-y-1 font-mono text-xs">
            <span>ref {d.ref || "–"}</span>
            <span>commit {shortSha(d.commit)}</span>
            <span>via {d.source}</span>
            {d.rollback_of ? <span>rollback of #{d.rollback_of}</span> : null}
            <span>created {formatDateTime(d.created_at)}</span>
            {d.finished_at && <span>finished {formatDateTime(d.finished_at)}</span>}
          </div>
        </SheetDescription>
      </SheetHeader>

      {d.error && <FailureBox deploy={d} />}

      <div className="flex items-center justify-between gap-2">
        <span className="label-mono">
          {log.lines.length} lines{log.dropped > 0 ? ` (+${log.dropped} older dropped)` : ""}
          {live && conn === "suspended" ? " · reconnecting" : ""}
        </span>
        <Button size="sm" variant={follow ? "secondary" : "outline"} aria-pressed={follow} onClick={() => setFollow((f) => !f)}>
          Follow
        </Button>
      </div>
      <div
        ref={box}
        tabIndex={0}
        role="log"
        aria-label={`Log of deploy ${d.id}`}
        onWheel={() => setFollow(false)}
        className="min-h-0 flex-1 overflow-auto rounded-md border bg-background p-3 font-mono text-xs leading-5"
      >
        {log.lines.length === 0 ? (
          <p className="text-muted-foreground">{conn === "pending" ? "Loading log…" : "No log output yet."}</p>
        ) : (
          log.lines.map((l, i) => (
            <div
              key={l.seq ?? `g${i}`}
              className={cn(
                "whitespace-pre-wrap break-all",
                l.stream === "stderr" && "border-l-2 border-muted-foreground/40 pl-2 text-muted-foreground",
                l.stream === "agent" && "font-medium text-primary",
                l.gap && "text-muted-foreground",
              )}
            >
              {l.text}
            </div>
          ))
        )}
      </div>
    </>
  );
}

function FailureBox({ deploy }: { deploy: Deploy }) {
  const e = deploy.error;
  if (!e) return null;
  return (
    <div role="alert" className="grid gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm">
      <p className="font-medium text-destructive">{failureReasonText(e.reason)}</p>
      {e.detail && <p className="whitespace-pre-wrap break-words font-mono text-xs text-muted-foreground">{e.detail}</p>}
      {e.violations && e.violations.length > 0 && (
        <div className="overflow-hidden rounded-md border bg-card">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Service</TableHead>
                <TableHead>Rule</TableHead>
                <TableHead>Detail</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {e.violations.map((v, i) => (
                <TableRow key={i}>
                  <TableCell>
                    <Mono>{v.service || "–"}</Mono>
                  </TableCell>
                  <TableCell>
                    <Mono>{v.rule}</Mono>
                  </TableCell>
                  <TableCell className="whitespace-normal break-words">{v.detail}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}
