import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink, RefreshCw } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { ConfirmDialog } from "@/components/admin/confirm";
import { QueryBoundary, Section } from "@/components/admin/state";
import { Mono } from "@/components/admin/table";
import { shouldReloadAfterUpdate, updateBlocker, updateResultBadge } from "@/components/ops/logic";
import { PageHeader } from "@/components/page";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { errorMessage } from "@/lib/errors";
import { formatDateTime, formatRelative } from "@/lib/format";
import { useApi, useConnectionState, useRpc } from "@/lib/rpc-context";
import type { UpdateStatus } from "@/lib/types";

const KEY = ["updates", "status"] as const;

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="label-mono mb-1">{label}</dt>
      <dd className="break-words text-sm">{children}</dd>
    </div>
  );
}

/** Admin: check for agent releases on GitHub and install one from the panel. */
export default function Updates() {
  const api = useApi();
  const rpc = useRpc();
  const conn = useConnectionState();
  const qc = useQueryClient();
  const [confirming, setConfirming] = React.useState(false);
  // Agent version before the update was requested; set while we wait for the restart.
  const [watching, setWatching] = React.useState<string | null>(null);
  const sawDrop = React.useRef(false);

  const q = useQuery({
    queryKey: KEY,
    queryFn: () => api.updates.status(),
    refetchInterval: (query) => (watching !== null || query.state.data?.state === "requested" ? 2_000 : 60_000),
  });
  const state = q.data?.state;

  // An update requested elsewhere (or before this page opened) is watched too.
  React.useEffect(() => {
    if (state === "requested" && watching === null) setWatching(rpc.hello?.agent.version ?? "");
  }, [state, watching, rpc]);

  React.useEffect(() => {
    if (watching === null) return;
    if (conn !== "open") {
      sawDrop.current = true;
      return;
    }
    if (!sawDrop.current) return;
    if (shouldReloadAfterUpdate(watching, rpc.hello?.agent.version)) {
      window.location.reload();
    } else if (state === "idle") {
      sawDrop.current = false;
      setWatching(null); // restarted on the same version (e.g. rolled back): show the result
    }
  }, [conn, watching, rpc, state]);

  // Without a restart, the request ends when the agent leaves "requested" (helper missing, timeout).
  React.useEffect(() => {
    if (watching !== null && state === "idle" && conn === "open" && !sawDrop.current) setWatching(null);
  }, [watching, state, conn]);

  const check = useMutation({
    mutationFn: () => api.updates.check(),
    onSuccess: (s) => qc.setQueryData(KEY, s),
    onError: (e) => toast.error(errorMessage(e)),
  });

  const apply = async (s: UpdateStatus) => {
    if (!s.latest) return;
    const before = rpc.hello?.agent.version ?? s.current;
    await api.updates.apply({ version: s.latest.tag });
    sawDrop.current = false;
    setWatching(before);
    void qc.invalidateQueries({ queryKey: KEY });
  };

  return (
    <>
      <PageHeader
        eyebrow="Admin"
        title="Updates"
        description="Agent releases from GitHub. Updates are verified before they are installed."
        actions={
          <Button variant="outline" disabled={check.isPending || watching !== null} onClick={() => check.mutate()}>
            <RefreshCw className={check.isPending ? "animate-spin" : undefined} /> Check now
          </Button>
        }
      />
      <QueryBoundary query={q} what="update status">
        {(s) => {
          const blocker = updateBlocker(s);
          const busy = watching !== null || s.state === "requested";
          const restarting = busy && conn !== "open";
          return (
            <>
              {busy && (
                <Alert className="mb-6" role="status">
                  <div>
                    <AlertTitle>{restarting ? "Restarting…" : "Update requested"}</AlertTitle>
                    <AlertDescription>
                      {restarting
                        ? "The agent is restarting. This page reconnects and reloads by itself."
                        : "Waiting for the update helper to install the new version."}
                    </AlertDescription>
                  </div>
                </Alert>
              )}

              {s.check_error && (
                <Alert variant="destructive" className="mb-6">
                  <div>
                    <AlertTitle>Could not check for updates</AlertTitle>
                    <AlertDescription className="font-mono text-[13px]">{s.check_error}</AlertDescription>
                  </div>
                </Alert>
              )}

              <Section title="Version">
                <dl className="grid grid-cols-1 gap-x-6 gap-y-4 rounded-md border bg-card p-4 sm:grid-cols-2 lg:grid-cols-4">
                  <Fact label="Current">
                    <Mono>{s.current}</Mono>
                  </Fact>
                  <Fact label="Latest">
                    {s.latest ? (
                      <span className="flex items-center gap-2">
                        <Mono>{s.latest.tag}</Mono>
                        {s.update_available ? (
                          <Badge variant="success">update available</Badge>
                        ) : (
                          <Badge variant="outline">up to date</Badge>
                        )}
                      </span>
                    ) : (
                      "–"
                    )}
                  </Fact>
                  <Fact label="Channel">
                    <Badge variant="outline">{s.channel}</Badge>
                  </Fact>
                  <Fact label="Last checked">
                    {!s.check_enabled ? (
                      <span className="text-muted-foreground">checks are turned off</span>
                    ) : s.checked_at ? (
                      <span title={formatDateTime(s.checked_at)}>{formatRelative(s.checked_at)}</span>
                    ) : (
                      "not yet"
                    )}
                  </Fact>
                </dl>
              </Section>

              {s.last_result && (
                <Section title="Last update">
                  <div className="rounded-md border bg-card p-4 text-sm">
                    <div className="flex flex-wrap items-center gap-2">
                      <Badge variant={updateResultBadge(s.last_result.status).tone}>
                        {updateResultBadge(s.last_result.status).label}
                      </Badge>
                      <Mono>
                        {s.last_result.from} → {s.last_result.to}
                      </Mono>
                      <span className="text-muted-foreground" title={formatDateTime(s.last_result.finished_at)}>
                        {formatRelative(s.last_result.finished_at)}
                      </span>
                    </div>
                    {s.last_result.error && (
                      <Alert variant={s.last_result.status === "failed" ? "destructive" : "default"} className="mt-3">
                        <AlertDescription className="font-mono text-[13px]">{s.last_result.error}</AlertDescription>
                      </Alert>
                    )}
                  </div>
                </Section>
              )}

              {s.latest && (
                <Section
                  title="Release notes"
                  actions={
                    <Button asChild variant="outline" size="sm">
                      <a href={s.latest.url} target="_blank" rel="noopener noreferrer">
                        View on GitHub <ExternalLink />
                      </a>
                    </Button>
                  }
                >
                  <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-md border bg-card p-4 font-mono text-[13px]">
                    {s.latest.notes || "No release notes."}
                  </pre>
                </Section>
              )}

              <Section title="Install">
                <div className="flex flex-wrap items-center gap-3">
                  <Button disabled={blocker !== null} onClick={() => setConfirming(true)}>
                    Update now
                  </Button>
                  {blocker && <p className="max-w-prose text-sm text-muted-foreground">{blocker}</p>}
                </div>
              </Section>

              <ConfirmDialog
                open={confirming}
                onOpenChange={setConfirming}
                title={`Update to ${s.latest?.tag ?? "the latest version"}?`}
                description="The agent restarts. This panel reconnects and reloads by itself. Your containers keep running."
                confirmLabel="Update now"
                onConfirm={() => apply(s)}
              />
            </>
          );
        }}
      </QueryBoundary>
    </>
  );
}
