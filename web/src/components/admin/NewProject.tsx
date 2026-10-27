import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Plus, ShieldAlert, X } from "lucide-react";
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { Field, Textarea } from "@/components/admin/fields";
import { CopyButton } from "@/components/copy-button";
import { Alert, AlertDescription } from "@/components/ui/alert";
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { composeInvalid } from "@/lib/compose";
import { errorMessage, toastError } from "@/lib/errors";
import {
  gitFormError,
  gitFormToParams,
  imageFormError,
  imageFormToParams,
  isSshUrl,
  validSlug,
  type EnvRow,
  type GitForm,
  type ImageForm,
  type VolumeRow,
} from "@/lib/newproject";
import { useApi } from "@/lib/rpc-context";
import type { PolicyViolation, Project, ProjectCreateParams } from "@/lib/types";
import { Rejection } from "@/pages/admin/ProjectCompose";

type Kind = "image" | "git" | "upload";

interface Created {
  project: Project;
  webhookSecret?: string;
}

const emptyImage: ImageForm = {
  slug: "",
  image: "",
  service: "",
  port: "",
  domains: "",
  path: "",
  volumes: [],
  env: [],
};
const emptyGit: GitForm = { repoUrl: "", slug: "", mode: "branch", ref: "", composeFile: "", token: "", tokenUser: "" };

/** Admin: the "New project" button and its dialog (image, Git repository or compose upload). */
export function NewProjectButton() {
  const [open, setOpen] = React.useState(false);
  return (
    <>
      <Button onClick={() => setOpen(true)}>
        <Plus /> New project
      </Button>
      {open && <NewProjectDialog onClose={() => setOpen(false)} />}
    </>
  );
}

function NewProjectDialog({ onClose }: { onClose: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [kind, setKind] = React.useState<Kind>("image");
  const [img, setImg] = React.useState<ImageForm>(emptyImage);
  const [git, setGit] = React.useState<GitForm>(emptyGit);
  const [upload, setUpload] = React.useState({ slug: "", compose: "" });
  const [error, setError] = React.useState<string | null>(null);
  const [rejected, setRejected] = React.useState<{ detail: string; violations: PolicyViolation[] } | null>(null);
  const [created, setCreated] = React.useState<Created | null>(null);

  const open = (slug: string) => {
    onClose();
    navigate(`/projects/${slug}`);
  };

  const m = useMutation({
    mutationFn: (params: ProjectCreateParams) => api.projects.create(params),
    onMutate: () => {
      setError(null);
      setRejected(null);
    },
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ["projects"] });
      toast.success(`Project ${r.project.slug} created`);
      if (r.webhook_secret || r.project.status === "pending_access") {
        setCreated({ project: r.project, webhookSecret: r.webhook_secret });
      } else open(r.project.slug);
    },
    onError: (e) => {
      const invalid = composeInvalid(e);
      if (invalid) setRejected(invalid);
      else setError(errorMessage(e));
    },
  });

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    let problem: string | null;
    let params: ProjectCreateParams;
    if (kind === "image") {
      problem = imageFormError(img);
      params = imageFormToParams(img);
    } else if (kind === "git") {
      problem = gitFormError(git);
      params = gitFormToParams(git);
    } else {
      problem = !validSlug(upload.slug.trim())
        ? "Slug: lowercase letters, digits, - and _ (start with a letter or digit)"
        : upload.compose.trim() === ""
          ? "Add the compose file content"
          : null;
      params = { slug: upload.slug.trim(), compose: upload.compose };
    }
    if (problem) {
      setError(problem);
      setRejected(null);
      return;
    }
    m.mutate(params);
  };

  if (created) {
    return <CreatedDialog created={created} onOpen={() => open(created.project.slug)} onClose={onClose} />;
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>New project</DialogTitle>
          <DialogDescription>Deploy a container image, a Git repository or a Compose file.</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <Tabs
            value={kind}
            onValueChange={(v) => {
              setKind(v as Kind);
              setError(null);
              setRejected(null);
            }}
          >
            <TabsList aria-label="Project source">
              <TabsTrigger value="image">From an image</TabsTrigger>
              <TabsTrigger value="git">From a Git repository</TabsTrigger>
              <TabsTrigger value="upload">Upload a Compose file</TabsTrigger>
            </TabsList>
            <TabsContent value="image">
              <ImageFields form={img} onChange={setImg} />
            </TabsContent>
            <TabsContent value="git">
              <GitFields form={git} onChange={setGit} />
            </TabsContent>
            <TabsContent value="upload">
              <UploadFields value={upload} onChange={setUpload} />
            </TabsContent>
          </Tabs>
          {error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          {rejected && (
            <Rejection title="The project was rejected" detail={rejected.detail} violations={rejected.violations} />
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={m.isPending}>
              {m.isPending ? "Creating…" : "Create project"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function ImageFields({ form, onChange }: { form: ImageForm; onChange: (f: ImageForm) => void }) {
  const set = <K extends keyof ImageForm>(k: K, v: ImageForm[K]) => onChange({ ...form, [k]: v });
  return (
    <div className="grid gap-4 pt-2">
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="np-slug" label="Slug">
          <Input id="np-slug" value={form.slug} onChange={(e) => set("slug", e.target.value)} autoComplete="off" />
        </Field>
        <Field id="np-port" label="Port (optional)" hint="Container port to publish.">
          <Input
            id="np-port"
            inputMode="numeric"
            value={form.port}
            onChange={(e) => set("port", e.target.value)}
            autoComplete="off"
          />
        </Field>
      </div>
      <Field
        id="np-image"
        label="Image"
        hint="Full reference incl. registry and a tag, e.g. ghcr.io/acme/web:1.4.2"
      >
        <Input
          id="np-image"
          value={form.image}
          onChange={(e) => set("image", e.target.value)}
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          className="font-mono"
        />
      </Field>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="np-domains" label="Domains (optional)" hint="Comma separated. Requires a port.">
          <Input
            id="np-domains"
            value={form.domains}
            onChange={(e) => set("domains", e.target.value)}
            autoComplete="off"
            autoCapitalize="none"
          />
        </Field>
        <Field id="np-path" label="Path (optional)">
          <Input id="np-path" value={form.path} onChange={(e) => set("path", e.target.value)} autoComplete="off" />
        </Field>
      </div>
      <VolumeRows rows={form.volumes} onChange={(v) => set("volumes", v)} />
      <EnvRows rows={form.env} onChange={(v) => set("env", v)} />
      <details className="text-sm">
        <summary className="cursor-pointer text-muted-foreground">Advanced</summary>
        <div className="mt-3">
          <Field id="np-service" label="Service name" hint="Defaults to app.">
            <Input
              id="np-service"
              value={form.service}
              onChange={(e) => set("service", e.target.value)}
              autoComplete="off"
              autoCapitalize="none"
            />
          </Field>
        </div>
      </details>
    </div>
  );
}

function RowButton({ label, onClick }: { label: string; onClick: () => void }) {
  return (
    <Button type="button" variant="ghost" size="sm" onClick={onClick}>
      <X />
      <span className="sr-only">{label}</span>
    </Button>
  );
}

function VolumeRows({ rows, onChange }: { rows: VolumeRow[]; onChange: (r: VolumeRow[]) => void }) {
  const patch = (i: number, p: Partial<VolumeRow>) => onChange(rows.map((r, j) => (j === i ? { ...r, ...p } : r)));
  return (
    <fieldset className="grid gap-2">
      <legend className="text-sm font-medium">Volumes (optional)</legend>
      {rows.map((r, i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            aria-label={`Volume ${i + 1} name`}
            placeholder="name"
            value={r.name}
            onChange={(e) => patch(i, { name: e.target.value })}
            autoComplete="off"
            className="font-mono"
          />
          <Input
            aria-label={`Volume ${i + 1} container path`}
            placeholder="/path"
            value={r.path}
            onChange={(e) => patch(i, { path: e.target.value })}
            autoComplete="off"
            className="font-mono"
          />
          <label className="flex shrink-0 items-center gap-1.5 text-sm">
            <input type="checkbox" checked={r.readOnly} onChange={(e) => patch(i, { readOnly: e.target.checked })} />
            Read-only
          </label>
          <RowButton label={`Remove volume ${i + 1}`} onClick={() => onChange(rows.filter((_, j) => j !== i))} />
        </div>
      ))}
      <div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => onChange([...rows, { name: "", path: "", readOnly: false }])}
        >
          <Plus /> Add volume
        </Button>
      </div>
    </fieldset>
  );
}

function EnvRows({ rows, onChange }: { rows: EnvRow[]; onChange: (r: EnvRow[]) => void }) {
  const patch = (i: number, p: Partial<EnvRow>) => onChange(rows.map((r, j) => (j === i ? { ...r, ...p } : r)));
  return (
    <fieldset className="grid gap-2">
      <legend className="text-sm font-medium">Environment (optional)</legend>
      <p className="text-xs text-muted-foreground">
        Plain values only. Secrets go in Environment after creating the project.
      </p>
      {rows.map((r, i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            aria-label={`Variable ${i + 1} name`}
            placeholder="KEY"
            value={r.key}
            onChange={(e) => patch(i, { key: e.target.value })}
            autoComplete="off"
            autoCapitalize="none"
            className="font-mono"
          />
          <Input
            aria-label={`Variable ${i + 1} value`}
            placeholder="value"
            value={r.value}
            onChange={(e) => patch(i, { value: e.target.value })}
            autoComplete="off"
            className="font-mono"
          />
          <RowButton label={`Remove variable ${i + 1}`} onClick={() => onChange(rows.filter((_, j) => j !== i))} />
        </div>
      ))}
      <div>
        <Button type="button" variant="outline" size="sm" onClick={() => onChange([...rows, { key: "", value: "" }])}>
          <Plus /> Add variable
        </Button>
      </div>
    </fieldset>
  );
}

function GitFields({ form, onChange }: { form: GitForm; onChange: (f: GitForm) => void }) {
  const set = <K extends keyof GitForm>(k: K, v: GitForm[K]) => onChange({ ...form, [k]: v });
  const ssh = isSshUrl(form.repoUrl);
  return (
    <div className="grid gap-4 pt-2">
      <Field
        id="np-repo"
        label="Repository URL"
        hint="An https:// URL, or an SSH URL (the agent then generates a deploy key for you to add)."
      >
        <Input
          id="np-repo"
          value={form.repoUrl}
          onChange={(e) => set("repoUrl", e.target.value)}
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          className="font-mono"
        />
      </Field>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="np-git-slug" label="Slug (optional)" hint="Defaults to the repository name.">
          <Input id="np-git-slug" value={form.slug} onChange={(e) => set("slug", e.target.value)} autoComplete="off" />
        </Field>
        <Field id="np-compose-file" label="Compose file (optional)" hint="Path in the repository.">
          <Input
            id="np-compose-file"
            value={form.composeFile}
            onChange={(e) => set("composeFile", e.target.value)}
            autoComplete="off"
            className="font-mono"
          />
        </Field>
      </div>
      <div className="grid gap-4 sm:grid-cols-[10rem_1fr]">
        <Field id="np-mode" label="Deploy on">
          <select
            id="np-mode"
            value={form.mode}
            onChange={(e) => set("mode", e.target.value as GitForm["mode"])}
            className="h-9 w-full rounded-md border border-input bg-card px-3 text-sm"
          >
            <option value="branch">Branch</option>
            <option value="tag_pattern">Tag pattern</option>
          </select>
        </Field>
        <Field
          id="np-ref"
          label={form.mode === "branch" ? "Branch (optional)" : "Tag pattern"}
          hint={form.mode === "branch" ? "Defaults to the repository's default branch." : "A glob such as v*."}
        >
          <Input
            id="np-ref"
            value={form.ref}
            onChange={(e) => set("ref", e.target.value)}
            autoComplete="off"
            autoCapitalize="none"
            className="font-mono"
          />
        </Field>
      </div>
      {!ssh && (
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="np-token" label="Access token (private HTTPS repositories)">
            <Input
              id="np-token"
              type="password"
              value={form.token}
              onChange={(e) => set("token", e.target.value)}
              autoComplete="new-password"
            />
          </Field>
          <Field id="np-token-user" label="Token user (optional)">
            <Input
              id="np-token-user"
              value={form.tokenUser}
              onChange={(e) => set("tokenUser", e.target.value)}
              autoComplete="off"
            />
          </Field>
        </div>
      )}
    </div>
  );
}

function UploadFields({
  value,
  onChange,
}: {
  value: { slug: string; compose: string };
  onChange: (v: { slug: string; compose: string }) => void;
}) {
  const onFile = async (f: File | undefined) => {
    if (!f) return;
    try {
      onChange({ ...value, compose: await f.text() });
    } catch (e) {
      toastError("Could not read the file", e);
    }
  };
  return (
    <div className="grid gap-4 pt-2">
      <Field id="np-up-slug" label="Slug">
        <Input
          id="np-up-slug"
          value={value.slug}
          onChange={(e) => onChange({ ...value, slug: e.target.value })}
          autoComplete="off"
        />
      </Field>
      <Field id="np-up-file" label="Load from a file (optional)">
        <input
          id="np-up-file"
          type="file"
          accept=".yml,.yaml,text/yaml"
          onChange={(e) => void onFile(e.target.files?.[0])}
          className="text-sm"
        />
      </Field>
      <Field id="np-up-compose" label="Compose file" hint="Validated against the agent's policy before it is stored.">
        <Textarea
          id="np-up-compose"
          value={value.compose}
          onChange={(e) => onChange({ ...value, compose: e.target.value })}
          rows={12}
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
        />
      </Field>
    </div>
  );
}

function SecretRow({ label, value, mono = "text-[13px]" }: { label: string; value: string; mono?: string }) {
  return (
    <div className="grid gap-1.5">
      <p className="label-mono">{label}</p>
      <div className="flex items-start gap-2">
        <code className={`min-w-0 flex-1 break-all rounded-md border bg-muted px-2.5 py-2 font-mono ${mono}`}>{value}</code>
        <CopyButton value={value} label={label} />
      </div>
    </div>
  );
}

/** Shown after creating a project that needs a follow-up: the deploy key to add and/or the webhook secret. */
function CreatedDialog({ created, onOpen, onClose }: { created: Created; onOpen: () => void; onClose: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const { project, webhookSecret } = created;
  const [status, setStatus] = React.useState(project.status);
  const [detail, setDetail] = React.useState(project.status_detail);
  const verify = useMutation({
    mutationFn: () => api.projects.verify({ slug: project.slug }),
    onSuccess: (p) => {
      setStatus(p.status);
      setDetail(p.status_detail);
      void qc.invalidateQueries({ queryKey: ["projects"] });
      void qc.invalidateQueries({ queryKey: ["project", project.slug] });
      if (p.status === "ready") toast.success("Access verified");
    },
    onError: (e) => toastError("Could not verify access", e),
  });
  const key = project.auth.public_key;
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{project.slug} created</DialogTitle>
          <DialogDescription>
            {key ? "Add the deploy key to the repository, then verify access." : "Keep the webhook secret for your Git host."}
          </DialogDescription>
        </DialogHeader>
        {key && (
          <>
            <SecretRow label="Deploy key" value={key} mono="text-xs" />
            <p className="text-xs text-muted-foreground">
              Add it as a read-only deploy key in the repository settings.
            </p>
            <div className="flex flex-wrap items-center gap-3">
              <Button type="button" variant="outline" disabled={verify.isPending} onClick={() => verify.mutate()}>
                {verify.isPending ? "Verifying…" : "Verify access"}
              </Button>
              <span className="text-sm text-muted-foreground">
                {status === "ready" ? "Access verified" : detail || "Waiting for access"}
              </span>
            </div>
          </>
        )}
        {webhookSecret && (
          <>
            <Alert variant="warning">
              <ShieldAlert />
              <AlertDescription>The webhook secret is shown only once. Copy it now.</AlertDescription>
            </Alert>
            <SecretRow label="Webhook secret" value={webhookSecret} />
          </>
        )}
        <DialogFooter>
          <Button onClick={onOpen}>Open project</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
