import * as React from "react";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { subscribeContainerLogs } from "@/lib/api";
import { useSubscription } from "@/lib/rpc-context";
import type { SubscriptionState } from "@/lib/rpc";
import { cn } from "@/lib/utils";

export interface LogTarget {
  id: string;
  name: string;
}

interface Row {
  n: number;
  time?: string;
  stream: "stdout" | "stderr" | "note";
  text: string;
}

const MAX_ROWS = 2000;

/** Live container logs in a side drawer (containers.logs.subscribe, last 200 lines then follow). */
export function ContainerLogsSheet({ target, onClose }: { target: LogTarget | null; onClose: () => void }) {
  return (
    <Sheet open={target !== null} onOpenChange={(o) => !o && onClose()}>
      <SheetContent aria-describedby="logs-desc">
        {target && <LogsBody key={target.id} target={target} />}
      </SheetContent>
    </Sheet>
  );
}

function LogsBody({ target }: { target: LogTarget }) {
  const [rows, setRows] = React.useState<Row[]>([]);
  const [state, setState] = React.useState<SubscriptionState>("pending");
  const [follow, setFollow] = React.useState(true);
  const pending = React.useRef<Row[]>([]);
  const counter = React.useRef(0);
  const timer = React.useRef<ReturnType<typeof setTimeout> | null>(null);
  const box = React.useRef<HTMLDivElement>(null);

  const flush = React.useCallback(() => {
    timer.current = null;
    const batch = pending.current;
    pending.current = [];
    if (batch.length > 0) setRows((prev) => [...prev, ...batch].slice(-MAX_ROWS));
  }, []);

  const push = React.useCallback(
    (row: Omit<Row, "n">) => {
      pending.current.push({ ...row, n: ++counter.current });
      timer.current ??= setTimeout(flush, 100);
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
      subscribeContainerLogs(
        rpc,
        { id: target.id, tail: 200 },
        {
          onData: (d) => {
            if ("eof" in d) push({ stream: "note", text: d.error ? `-- log stream ended: ${d.error} --` : "-- end of log --" });
            else push({ stream: d.stream, time: d.time, text: d.text });
          },
          onGap: () => push({ stream: "note", text: "-- some log lines were skipped --" }),
          onState: setState,
        },
      ),
    [target.id],
  );

  React.useEffect(() => {
    const el = box.current;
    if (follow && el) el.scrollTop = el.scrollHeight;
  }, [rows, follow]);

  return (
    <>
      <SheetHeader>
        <SheetTitle className="font-mono text-base">{target.name}</SheetTitle>
        <SheetDescription id="logs-desc">
          Live container log. {state === "suspended" ? "Reconnecting…" : state === "active" ? "Following." : ""}
        </SheetDescription>
      </SheetHeader>
      <div className="flex items-center justify-between gap-2">
        <span className="label-mono">{rows.length} lines</span>
        <div className="flex gap-2">
          <Button size="sm" variant={follow ? "secondary" : "outline"} aria-pressed={follow} onClick={() => setFollow((f) => !f)}>
            Follow
          </Button>
          <Button size="sm" variant="outline" onClick={() => setRows([])}>
            Clear
          </Button>
        </div>
      </div>
      <div
        ref={box}
        tabIndex={0}
        role="log"
        aria-label={`Logs of ${target.name}`}
        onWheel={() => setFollow(false)}
        className="min-h-0 flex-1 overflow-auto rounded-md border bg-background p-3 font-mono text-xs leading-5"
      >
        {rows.length === 0 ? (
          <p className="text-muted-foreground">{state === "pending" ? "Connecting…" : "No log output yet."}</p>
        ) : (
          rows.map((r) => (
            <div
              key={r.n}
              className={cn(
                "whitespace-pre-wrap break-all",
                r.stream === "stderr" && "border-l-2 border-muted-foreground/40 pl-2 text-muted-foreground",
                r.stream === "note" && "text-muted-foreground",
              )}
            >
              {r.time && <span className="mr-2 text-muted-foreground">{r.time.slice(11, 19)}</span>}
              {r.text}
            </div>
          ))
        )}
      </div>
    </>
  );
}
