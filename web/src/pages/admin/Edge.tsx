import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { FilePlus2, Trash2 } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { ConfirmDialog } from "@/components/admin/confirm";
import { Field, Textarea } from "@/components/admin/fields";
import { EmptyState, QueryBoundary, Section } from "@/components/admin/state";
import { DataTable, Mono, Td, Th, Tr } from "@/components/admin/table";
import { CopyButton } from "@/components/copy-button";
import { PageHeader } from "@/components/page";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { errorMessage } from "@/lib/errors";
import { formatDateTime, formatRelative } from "@/lib/format";
import { useApi } from "@/lib/rpc-context";
import type { EdgeStatus } from "@/lib/types";

const KEY = ["admin", "edge"] as const;

const STATE_VARIANT = { running: "success", starting: "warning", error: "destructive", disabled: "outline" } as const;

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="label-mono mb-1">{label}</dt>
      <dd className="break-words text-sm">{children}</dd>
    </div>
  );
}

/** Admin: reverse proxy status, certificates, CSR and install flow. */
export default function Edge() {
  const api = useApi();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: KEY, queryFn: () => api.edge.status(), refetchInterval: 15_000 });
  const [installing, setInstalling] = React.useState<string | null | false>(false);
  const [removing, setRemoving] = React.useState<string | null>(null);
  const set = (s: EdgeStatus) => qc.setQueryData(KEY, s);

  return (
    <>
      <PageHeader
        eyebrow="Admin"
        title="Edge"
        description="The reverse proxy that serves the panel and project domains."
        actions={
          <Button variant="outline" onClick={() => setInstalling(null)}>
            <FilePlus2 /> Install certificate
          </Button>
        }
      />
      <QueryBoundary query={q} what="edge status">
        {(s) => (
          <>
            {s.last_error && (
              <Alert variant="destructive" className="mb-6">
                <div>
                  <AlertTitle>Last error</AlertTitle>
                  <AlertDescription className="font-mono text-[13px]">{s.last_error}</AlertDescription>
                </div>
              </Alert>
            )}

            <Section title="Status">
              <dl className="grid grid-cols-1 gap-x-6 gap-y-4 rounded-md border bg-card p-4 sm:grid-cols-2 lg:grid-cols-3">
                <Fact label="State">
                  <Badge variant={STATE_VARIANT[s.state]}>{s.state}</Badge>
                  {!s.enabled && <span className="ml-2 text-muted-foreground">edge is turned off</span>}
                </Fact>
                <Fact label="Ports">
                  <Mono>
                    http {s.http_port} · https {s.https_port}
                  </Mono>
                </Fact>
                <Fact label="Routes">
                  <Mono>{s.routes}</Mono>
                </Fact>
                <Fact label="Image">
                  <Mono className="break-all">{s.image || "–"}</Mono>
                </Fact>
                <Fact label="Server hostnames">
                  {s.server_hostnames.length === 0 ? (
                    "–"
                  ) : (
                    <span className="flex flex-wrap gap-1.5">
                      {s.server_hostnames.map((h) => (
                        <Mono key={h} className="rounded-sm border px-1.5 py-0.5">
                          {h}
                        </Mono>
                      ))}
                    </span>
                  )}
                </Fact>
                <Fact label="Self-signed certificate">
                  <div className="flex items-start gap-2">
                    <Mono className="min-w-0 break-all text-xs">{s.self_signed.fingerprint_sha256}</Mono>
                    <CopyButton value={s.self_signed.fingerprint_sha256} label="fingerprint" />
                  </div>
                  <p className="mt-1 text-xs text-muted-foreground">
                    expires {formatDateTime(s.self_signed.not_after)} ({formatRelative(s.self_signed.not_after)})
                  </p>
                </Fact>
              </dl>
            </Section>

            <Section
              title="Certificates"
              description="Hostnames with a certificate of their own. Others are served with the self-signed one."
            >
              {s.certificates.length === 0 ? (
                <EmptyState>No certificates</EmptyState>
              ) : (
                <DataTable>
                  <thead>
                    <tr>
                      <Th>Hostname</Th>
                      <Th>Source</Th>
                      <Th>Expires</Th>
                      <Th className="text-right">Actions</Th>
                    </tr>
                  </thead>
                  <tbody>
                    {s.certificates.map((c) => (
                      <Tr key={c.hostname}>
                        <Td>
                          <Mono className="font-medium">{c.hostname}</Mono>
                        </Td>
                        <Td>
                          <Badge variant={c.source === "acme" ? "secondary" : "outline"}>{c.source}</Badge>
                        </Td>
                        <Td className="whitespace-nowrap" title={formatDateTime(c.not_after)}>
                          {c.not_after ? formatRelative(c.not_after) : "–"}
                        </Td>
                        <Td>
                          <div className="flex justify-end gap-1.5">
                            <Button size="sm" variant="outline" onClick={() => setInstalling(c.hostname)}>
                              Replace<span className="sr-only"> certificate of {c.hostname}</span>
                            </Button>
                            {c.source === "installed" && (
                              <Button size="sm" variant="outline" onClick={() => setRemoving(c.hostname)}>
                                <Trash2 /> Remove<span className="sr-only"> {c.hostname}</span>
                              </Button>
                            )}
                          </div>
                        </Td>
                      </Tr>
                    ))}
                  </tbody>
                </DataTable>
              )}
            </Section>
          </>
        )}
      </QueryBoundary>

      <InstallCertDialog
        hostname={installing === false ? null : installing}
        open={installing !== false}
        onOpenChange={(o) => !o && setInstalling(false)}
        onInstalled={(s, host) => {
          set(s);
          setInstalling(false);
          toast.success(`Certificate installed for ${host}`);
        }}
      />

      <ConfirmDialog
        open={removing !== null}
        onOpenChange={(o) => !o && setRemoving(null)}
        title={`Remove certificate for ${removing ?? ""}?`}
        description="The installed certificate and its key are deleted. The hostname falls back to the self-signed certificate (or ACME, where configured)."
        confirmLabel="Remove certificate"
        onConfirm={async () => {
          if (!removing) return;
          set(await api.edge.removeCertificate({ hostname: removing }));
          toast.success(`Removed certificate for ${removing}`);
        }}
      />
    </>
  );
}

/** Two steps: generate a CSR for a hostname, then paste the signed certificate. */
function InstallCertDialog({
  hostname: initial,
  open,
  onOpenChange,
  onInstalled,
}: {
  hostname: string | null;
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onInstalled: (s: EdgeStatus, hostname: string) => void;
}) {
  const api = useApi();
  const [hostname, setHostname] = React.useState("");
  const [csr, setCsr] = React.useState("");
  const [pem, setPem] = React.useState("");
  React.useEffect(() => {
    if (open) {
      setHostname(initial ?? "");
      setCsr("");
      setPem("");
      gen.reset();
      install.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, initial]);

  const gen = useMutation({
    mutationFn: () => api.edge.csr({ hostname: hostname.trim() }),
    onSuccess: (r) => {
      setCsr(r.csr);
      toast.success(`CSR generated for ${r.hostname}`);
    },
  });
  const install = useMutation({
    mutationFn: () => api.edge.installCertificate({ hostname: hostname.trim(), certificate: pem.trim() }),
    onSuccess: (s) => onInstalled(s, hostname.trim()),
  });
  const err = gen.error ?? install.error;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Install certificate</DialogTitle>
          <DialogDescription>
            Generate a signing request, have your CA sign it, then paste the certificate chain. The private key never
            leaves the agent.
          </DialogDescription>
        </DialogHeader>

        <Field id="cert-host" label="1. Hostname">
          <div className="flex gap-2">
            <Input
              id="cert-host"
              value={hostname}
              onChange={(e) => {
                setHostname(e.target.value);
                setCsr("");
              }}
              placeholder="app.example.com"
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              className="font-mono"
            />
            <Button
              type="button"
              variant="outline"
              disabled={gen.isPending || hostname.trim() === ""}
              onClick={() => gen.mutate()}
            >
              {gen.isPending ? "Generating…" : csr ? "Regenerate CSR" : "Generate CSR"}
            </Button>
          </div>
        </Field>

        {csr && (
          <div className="grid gap-1.5">
            <div className="flex items-center justify-between gap-2">
              <p className="label-mono">Certificate signing request</p>
              <CopyButton value={csr} label="CSR" />
            </div>
            <Textarea readOnly value={csr} rows={6} aria-label="Certificate signing request" onFocus={(e) => e.currentTarget.select()} />
          </div>
        )}

        <Field id="cert-pem" label="2. Signed certificate (PEM)" hint="Leaf first, then intermediates.">
          <Textarea
            id="cert-pem"
            value={pem}
            onChange={(e) => setPem(e.target.value)}
            rows={6}
            placeholder="-----BEGIN CERTIFICATE-----"
            spellCheck={false}
          />
        </Field>

        {err && (
          <Alert variant="destructive">
            <AlertDescription>{errorMessage(err)}</AlertDescription>
          </Alert>
        )}
        <DialogFooter>
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            type="button"
            disabled={install.isPending || hostname.trim() === "" || pem.trim() === ""}
            onClick={() => install.mutate()}
          >
            {install.isPending ? "Installing…" : "Install certificate"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
