import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Rocket } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { ConfirmDialog } from "@/components/admin/confirm";
import { Field, Textarea } from "@/components/admin/fields";
import { EmptyState, QueryBoundary, Section } from "@/components/admin/state";
import { DataTable, Mono, Td, Th, Tr } from "@/components/admin/table";
import { CopyButton } from "@/components/copy-button";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useCan } from "@/lib/auth-context";
import { composeInvalid, composeRef } from "@/lib/compose";
import { toastError } from "@/lib/errors";
import { formatDateTime, formatRelative } from "@/lib/format";
import { useApi } from "@/lib/rpc-context";
import type { PolicyViolation } from "@/lib/types";

/** Admin: the stored compose versions of a compose-sourced project, and a editor for the next one. */
export function ProjectCompose({ slug, ready }: { slug: string; ready: boolean }) {
  const api = useApi();
  const qc = useQueryClient();
  const canOperate = useCan("operator");
  const history = useQuery({
    queryKey: ["compose", slug, "history"],
    queryFn: () => api.projects.composeHistory({ slug, limit: 50 }),
  });
  const current = useQuery({
    queryKey: ["compose", slug, "current"],
    queryFn: () => api.projects.composeGet({ slug }),
    retry: false,
  });
  const [deploying, setDeploying] = React.useState<number | null>(null);

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ["compose", slug] });
    void qc.invalidateQueries({ queryKey: ["project", slug] });
    void qc.invalidateQueries({ queryKey: ["projects"] });
    void qc.invalidateQueries({ queryKey: ["deploys", slug] });
  };

  return (
    <div>
      <Section
        title={current.data ? `Current version (v${current.data.version})` : "Current version"}
        description="Deploys use the stored version you pick; saving a new version does not change what is running."
      >
        <QueryBoundary query={current} what="the compose content">
          {(c) => (
            <div className="grid gap-2">
              <div className="flex justify-end">
                <CopyButton value={c.content} label="Compose content" />
              </div>
              <pre
                tabIndex={0}
                aria-label={`Compose content of version ${c.version}`}
                className="max-h-96 overflow-auto rounded-md border bg-card p-3 font-mono text-[13px] leading-relaxed"
              >
                {c.content}
              </pre>
            </div>
          )}
        </QueryBoundary>
      </Section>

      <Section title="History">
        <QueryBoundary query={history} what="the version history">
          {(rows) =>
            rows.length === 0 ? (
              <EmptyState>No versions yet</EmptyState>
            ) : (
              <DataTable>
                <thead>
                  <tr>
                    <Th>Version</Th>
                    <Th>Created</Th>
                    <Th>By</Th>
                    <Th>Note</Th>
                    <Th>Deployed</Th>
                    {canOperate && <Th className="text-right">Actions</Th>}
                  </tr>
                </thead>
                <tbody>
                  {rows.map((v) => (
                    <Tr key={v.version}>
                      <Td>
                        <Mono className="font-medium">v{v.version}</Mono>
                      </Td>
                      <Td className="whitespace-nowrap text-muted-foreground" title={formatDateTime(v.created_at)}>
                        {formatRelative(v.created_at)}
                      </Td>
                      <Td>
                        <Mono>{v.created_by || "–"}</Mono>
                      </Td>
                      <Td className="max-w-xs whitespace-normal break-words">{v.note || "–"}</Td>
                      <Td>{v.deployed ? <Badge variant="secondary">Deployed</Badge> : <span className="text-muted-foreground">–</span>}</Td>
                      {canOperate && (
                        <Td>
                          <div className="flex justify-end">
                            <Button size="sm" variant="outline" disabled={!ready} onClick={() => setDeploying(v.version)}>
                              <Rocket /> Deploy this version<span className="sr-only"> v{v.version}</span>
                            </Button>
                          </div>
                        </Td>
                      )}
                    </Tr>
                  ))}
                </tbody>
              </DataTable>
            )
          }
        </QueryBoundary>
      </Section>

      <NewVersion slug={slug} initial={current.data?.content} canDeploy={canOperate && ready} onSaved={refresh} />

      <ConfirmDialog
        open={deploying !== null}
        onOpenChange={(o) => !o && setDeploying(null)}
        title={`Deploy v${deploying ?? ""}?`}
        description="Builds and starts the project from this stored compose version."
        confirmLabel="Deploy"
        onConfirm={async () => {
          if (deploying === null) return;
          const d = await api.deploys.trigger({ project: slug, ref: composeRef(deploying) });
          toast.success(`Deploy #${d.id} queued`, { description: `Compose v${deploying}` });
          refresh();
        }}
      />
    </div>
  );
}

function NewVersion({
  slug,
  initial,
  canDeploy,
  onSaved,
}: {
  slug: string;
  initial: string | undefined;
  canDeploy: boolean;
  onSaved: () => void;
}) {
  const api = useApi();
  const [text, setText] = React.useState("");
  const [note, setNote] = React.useState("");
  const [deploy, setDeploy] = React.useState(false);
  const [rejected, setRejected] = React.useState<{ detail: string; violations: PolicyViolation[] } | null>(null);
  const seeded = React.useRef(false);
  // Start from the current content once, so edits are a diff from what runs.
  React.useEffect(() => {
    if (initial !== undefined && !seeded.current) {
      seeded.current = true;
      setText(initial);
    }
  }, [initial]);

  const m = useMutation({
    mutationFn: () =>
      api.projects.composeSet({ slug, compose: text, note: note.trim() || undefined, deploy: deploy || undefined }),
    onMutate: () => setRejected(null),
    onSuccess: (r) => {
      toast.success(`Saved v${r.version}`, { description: r.deploy ? `Deploy #${r.deploy.id} queued` : undefined });
      setNote("");
      onSaved();
    },
    onError: (e) => {
      const invalid = composeInvalid(e);
      if (invalid) setRejected(invalid);
      else toastError("Could not save the compose file", e);
    },
  });

  return (
    <Section title="New version" description="Saves the content below as the next version. It is validated against the agent's policy first.">
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          m.mutate();
        }}
      >
        <Field id="compose-content" label="Compose file">
          <Textarea
            id="compose-content"
            value={text}
            onChange={(e) => setText(e.target.value)}
            rows={16}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            aria-invalid={rejected ? true : undefined}
          />
        </Field>
        {rejected && <Rejection detail={rejected.detail} violations={rejected.violations} />}
        <Field id="compose-note" label="Note (optional)">
          <input
            id="compose-note"
            value={note}
            onChange={(e) => setNote(e.target.value)}
            maxLength={200}
            autoComplete="off"
            className="h-9 w-full rounded-md border border-input bg-card px-3 text-sm"
          />
        </Field>
        {canDeploy && (
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={deploy} onChange={(e) => setDeploy(e.target.checked)} />
            Deploy after saving
          </label>
        )}
        <div>
          <Button type="submit" disabled={m.isPending || text.trim() === ""}>
            {m.isPending ? "Saving…" : deploy ? "Save and deploy" : "Save new version"}
          </Button>
        </div>
      </form>
    </Section>
  );
}

function Rejection({ detail, violations }: { detail: string; violations: PolicyViolation[] }) {
  return (
    <Alert variant="destructive" role="alert">
      <AlertDescription>
        <p className="font-medium">The compose file was rejected</p>
        {detail && <p className="mt-1 whitespace-pre-wrap break-words font-mono text-xs">{detail}</p>}
        {violations.length > 0 && (
          <div className="mt-2 overflow-hidden rounded-md border bg-card text-card-foreground">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Service</TableHead>
                  <TableHead>Rule</TableHead>
                  <TableHead>Detail</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {violations.map((v, i) => (
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
      </AlertDescription>
    </Alert>
  );
}
