import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, Pencil, Trash2 } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { ConfirmDialog } from "@/components/admin/confirm";
import { Field, Textarea } from "@/components/admin/fields";
import { EmptyState, QueryBoundary, Section } from "@/components/admin/state";
import { DataTable, Mono, Td, Th, Tr } from "@/components/admin/table";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { errorMessage } from "@/lib/errors";
import { useApi } from "@/lib/rpc-context";
import { RpcError } from "@/lib/rpc";
import { ErrorCode, type Exposure, type Project } from "@/lib/types";

/** Domain conflict (-32009 with {domain, project}) as a sentence; other errors via errorMessage. */
function exposeError(e: unknown): { title: string; text: string } {
  if (e instanceof RpcError && e.code === ErrorCode.AlreadyExists) {
    const d = e.data as { domain?: string; project?: string } | undefined;
    if (d?.domain) {
      return {
        title: "Domain already in use",
        text: `${d.domain} is already exposed by project “${d.project ?? "another project"}”. Remove it there first or pick another domain.`,
      };
    }
  }
  return { title: "Could not expose service", text: errorMessage(e) };
}

function splitDomains(s: string): string[] {
  return s
    .split(/[\s,]+/)
    .map((d) => d.trim())
    .filter(Boolean);
}

/** Admin: effective exposure of a project (compose labels + project overrides), expose/unexpose. */
export function ProjectExposure({ slug }: { slug: string }) {
  const api = useApi();
  const qc = useQueryClient();
  const key = ["admin", "exposure", slug] as const;
  const q = useQuery({ queryKey: key, queryFn: () => api.projects.get({ slug }) });
  const [removing, setRemoving] = React.useState<string | null>(null);
  const [draft, setDraft] = React.useState<Exposure | null>(null);
  const set = (p: Project) => {
    qc.setQueryData(key, p);
    void qc.invalidateQueries({ queryKey: ["admin", "edge"] });
  };

  return (
    <div>
      <QueryBoundary query={q} what="exposure">
        {(p) => (
          <>
            {p.exposure_warnings && p.exposure_warnings.length > 0 && (
              <Alert variant="warning" className="mb-6">
                <AlertTriangle />
                <div>
                  <AlertTitle>Exposure warnings</AlertTitle>
                  <ul className="mt-1 list-disc pl-4 text-sm">
                    {p.exposure_warnings.map((w) => (
                      <li key={w}>{w}</li>
                    ))}
                  </ul>
                </div>
              </Alert>
            )}

            <Section
              title="Effective exposure"
              description="What the edge routes to this project. Entries from compose labels are changed in the repository; project entries override them."
            >
              {!p.exposure || p.exposure.length === 0 ? (
                <EmptyState>Nothing exposed</EmptyState>
              ) : (
                <DataTable>
                  <thead>
                    <tr>
                      <Th>Service</Th>
                      <Th>Port</Th>
                      <Th>Domains</Th>
                      <Th>Path</Th>
                      <Th>Source</Th>
                      <Th className="text-right">Actions</Th>
                    </tr>
                  </thead>
                  <tbody>
                    {p.exposure.map((x) => (
                      <Tr key={x.service}>
                        <Td>
                          <Mono className="font-medium">{x.service}</Mono>
                        </Td>
                        <Td>
                          <Mono>{x.port}</Mono>
                        </Td>
                        <Td>
                          <div className="flex flex-col gap-0.5">
                            {x.domains.map((d) => (
                              <Mono key={d}>{d}</Mono>
                            ))}
                          </div>
                        </Td>
                        <Td>
                          <Mono>{x.path || "/"}</Mono>
                        </Td>
                        <Td>
                          <Badge variant={x.source === "project" ? "default" : "secondary"}>{x.source}</Badge>
                        </Td>
                        <Td>
                          <div className="flex justify-end gap-1.5">
                            <Button size="sm" variant="outline" onClick={() => setDraft(x)}>
                              <Pencil /> Edit<span className="sr-only"> {x.service}</span>
                            </Button>
                            {x.source === "project" && (
                              <Button size="sm" variant="outline" onClick={() => setRemoving(x.service)}>
                                <Trash2 /> Unexpose<span className="sr-only"> {x.service}</span>
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

            <ExposeForm slug={slug} draft={draft} onSaved={set} />
          </>
        )}
      </QueryBoundary>

      <ConfirmDialog
        open={removing !== null}
        onOpenChange={(o) => !o && setRemoving(null)}
        title={`Unexpose ${removing ?? ""}?`}
        description="The project's override is removed. The service is routed only if its compose labels still say so."
        confirmLabel="Unexpose"
        onConfirm={async () => {
          if (!removing) return;
          set(await api.projects.unexpose({ slug, service: removing }));
          toast.success(`Unexposed ${removing}`);
        }}
      />
    </div>
  );
}

function ExposeForm({
  slug,
  draft,
  onSaved,
}: {
  slug: string;
  draft: Exposure | null;
  onSaved: (p: Project) => void;
}) {
  const api = useApi();
  const [service, setService] = React.useState("");
  const [port, setPort] = React.useState("");
  const [domains, setDomains] = React.useState("");
  const [path, setPath] = React.useState("");
  const ref = React.useRef<HTMLDivElement>(null);

  // "Edit" on a table row loads it here.
  React.useEffect(() => {
    if (!draft) return;
    setService(draft.service);
    setPort(String(draft.port));
    setDomains(draft.domains.join("\n"));
    setPath(draft.path ?? "");
    ref.current?.scrollIntoView({ block: "nearest", behavior: "smooth" });
    document.getElementById("exp-domains")?.focus({ preventScroll: true });
  }, [draft]);

  const portNum = Number(port);
  const valid =
    service.trim() !== "" && Number.isInteger(portNum) && portNum >= 1 && portNum <= 65535 && splitDomains(domains).length > 0;

  const m = useMutation({
    mutationFn: () =>
      api.projects.expose({
        slug,
        service: service.trim(),
        port: portNum,
        domains: splitDomains(domains),
        path: path.trim() || undefined,
      }),
    onSuccess: (p) => {
      toast.success(`Exposed ${service.trim()}`, { description: "Routes are updated without a redeploy." });
      setService("");
      setPort("");
      setDomains("");
      setPath("");
      onSaved(p);
    },
  });
  const err = m.isError ? exposeError(m.error) : null;

  return (
    <div ref={ref}>
      <Section
        title="Expose a service"
        description="Routes domains to a service port. Saving a service that is already exposed replaces its entry."
      >
        <form
          className="grid max-w-2xl gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            m.mutate();
          }}
        >
          <div className="grid gap-4 sm:grid-cols-[1fr_8rem]">
            <Field id="exp-service" label="Service" hint="Compose service name.">
              <Input
                id="exp-service"
                value={service}
                onChange={(e) => setService(e.target.value)}
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                className="font-mono"
              />
            </Field>
            <Field id="exp-port" label="Port" hint="Container port.">
              <Input
                id="exp-port"
                inputMode="numeric"
                value={port}
                onChange={(e) => setPort(e.target.value.replace(/\D/g, ""))}
                autoComplete="off"
                className="font-mono"
              />
            </Field>
          </div>
          <Field id="exp-domains" label="Domains" hint="One per line, or separated by commas.">
            <Textarea
              id="exp-domains"
              value={domains}
              onChange={(e) => setDomains(e.target.value)}
              rows={3}
              placeholder="app.example.com"
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Field id="exp-path" label="Path prefix (optional)" hint="Only requests under this path are routed, e.g. /api.">
            <Input
              id="exp-path"
              value={path}
              onChange={(e) => setPath(e.target.value)}
              placeholder="/"
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
            />
          </Field>
          {err && (
            <Alert variant="destructive">
              <div>
                <AlertTitle>{err.title}</AlertTitle>
                <AlertDescription>{err.text}</AlertDescription>
              </div>
            </Alert>
          )}
          <div>
            <Button type="submit" disabled={m.isPending || !valid}>
              {m.isPending ? "Saving…" : "Expose service"}
            </Button>
          </div>
        </form>
      </Section>
    </div>
  );
}
