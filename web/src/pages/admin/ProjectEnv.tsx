import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Trash2, Upload } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { ConfirmDialog } from "@/components/admin/confirm";
import { Field, Textarea } from "@/components/admin/fields";
import { EmptyState, QueryBoundary, Section } from "@/components/admin/state";
import { DataTable, Mono, Td, Th, Tr } from "@/components/admin/table";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { parseDotenv } from "@/lib/admin";
import { toastError } from "@/lib/errors";
import { formatDateTime, formatRelative } from "@/lib/format";
import { useApi } from "@/lib/rpc-context";

/** Admin: environment variable names of a project. Values are write-only and never displayed. */
export function ProjectEnv({ slug }: { slug: string }) {
  const api = useApi();
  const qc = useQueryClient();
  const key = ["admin", "env", slug] as const;
  const q = useQuery({ queryKey: key, queryFn: () => api.projects.envList({ slug }) });
  const [unsetting, setUnsetting] = React.useState<string | null>(null);
  const refresh = () => qc.invalidateQueries({ queryKey: key });

  return (
    <div>
      <Alert variant="warning" className="mb-6">
        <AlertDescription>
          Values are stored encrypted and cannot be read back. Changes reach the containers on the <strong>next
          deploy</strong>: redeploy to apply.
        </AlertDescription>
      </Alert>

      <Section title="Variables">
        <QueryBoundary query={q} what="environment variables">
          {({ vars }) =>
            vars.length === 0 ? (
              <EmptyState>No environment variables</EmptyState>
            ) : (
              <DataTable>
                <thead>
                  <tr>
                    <Th>Name</Th>
                    <Th>Updated</Th>
                    <Th className="text-right">Actions</Th>
                  </tr>
                </thead>
                <tbody>
                  {vars.map((v) => (
                    <Tr key={v.name}>
                      <Td>
                        <Mono className="font-medium">{v.name}</Mono>
                      </Td>
                      <Td className="whitespace-nowrap text-muted-foreground" title={formatDateTime(v.updated_at)}>
                        {formatRelative(v.updated_at)}
                      </Td>
                      <Td>
                        <div className="flex justify-end">
                          <Button size="sm" variant="outline" onClick={() => setUnsetting(v.name)}>
                            <Trash2 /> Unset<span className="sr-only"> {v.name}</span>
                          </Button>
                        </div>
                      </Td>
                    </Tr>
                  ))}
                </tbody>
              </DataTable>
            )
          }
        </QueryBoundary>
      </Section>

      <div className="grid gap-8 lg:grid-cols-2">
        <SetVariable slug={slug} onDone={() => void refresh()} />
        <ImportDotenv slug={slug} onDone={() => void refresh()} />
      </div>

      <ConfirmDialog
        open={unsetting !== null}
        onOpenChange={(o) => !o && setUnsetting(null)}
        title={`Unset ${unsetting ?? ""}?`}
        description="The variable is deleted from the project. Running containers keep their current value until the next deploy."
        confirmLabel="Unset variable"
        onConfirm={async () => {
          if (!unsetting) return;
          await api.projects.envUnset({ slug, names: [unsetting] });
          toast.success(`Unset ${unsetting}`, { description: "Redeploy to apply." });
          void refresh();
        }}
      />
    </div>
  );
}

function SetVariable({ slug, onDone }: { slug: string; onDone: () => void }) {
  const api = useApi();
  const [name, setName] = React.useState("");
  const [value, setValue] = React.useState("");
  const m = useMutation({
    mutationFn: () => api.projects.envSet({ slug, vars: { [name.trim()]: value } }),
    onSuccess: () => {
      toast.success(`Set ${name.trim()}`, { description: "Redeploy to apply." });
      setName("");
      setValue(""); // write-only: nothing of the value stays in the page
      onDone();
    },
    onError: (e) => toastError(`Could not set ${name.trim()}`, e),
  });
  return (
    <Section title="Set a variable" description="Creates the variable or replaces its value.">
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          m.mutate();
        }}
      >
        <Field id="env-name" label="Name">
          <Input
            id="env-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="DATABASE_URL"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            className="font-mono"
          />
        </Field>
        <Field id="env-value" label="Value" hint="Write-only. Cleared after saving.">
          <Textarea
            id="env-value"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            rows={3}
            autoComplete="off"
            spellCheck={false}
          />
        </Field>
        <div>
          <Button type="submit" disabled={m.isPending || name.trim() === "" || value === ""}>
            {m.isPending ? "Saving…" : "Save variable"}
          </Button>
        </div>
      </form>
    </Section>
  );
}

function ImportDotenv({ slug, onDone }: { slug: string; onDone: () => void }) {
  const api = useApi();
  const [text, setText] = React.useState("");
  const parsed = React.useMemo(() => parseDotenv(text), [text]);
  const count = Object.keys(parsed.vars).length;
  const m = useMutation({
    mutationFn: () => api.projects.envSet({ slug, vars: parsed.vars }),
    onSuccess: () => {
      toast.success(`Imported ${count} variable${count === 1 ? "" : "s"}`, { description: "Redeploy to apply." });
      setText("");
      onDone();
    },
    onError: (e) => toastError("Could not import", e),
  });
  return (
    <Section
      title="Import a .env"
      description="Paste NAME=VALUE lines. Comments, “export ” prefixes and quotes are understood; variables not listed are left alone."
    >
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          m.mutate();
        }}
      >
        <Field id="env-import" label=".env contents">
          <Textarea
            id="env-import"
            value={text}
            onChange={(e) => setText(e.target.value)}
            rows={6}
            placeholder={"# comment\nKEY=value\nOTHER=\"quoted value\""}
            autoComplete="off"
            spellCheck={false}
            aria-invalid={parsed.error ? true : undefined}
          />
        </Field>
        {parsed.error ? (
          <p className="text-sm text-destructive" role="alert">
            {parsed.error}
          </p>
        ) : (
          text.trim() !== "" && (
            <p className="label-mono">
              {count} variable{count === 1 ? "" : "s"}: {Object.keys(parsed.vars).join(", ") || "none"}
            </p>
          )
        )}
        <div>
          <Button type="submit" disabled={m.isPending || !!parsed.error || count === 0}>
            <Upload /> {m.isPending ? "Importing…" : "Import variables"}
          </Button>
        </div>
      </form>
    </Section>
  );
}
