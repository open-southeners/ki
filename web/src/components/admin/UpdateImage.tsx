import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { PackageOpen } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { Field, NativeSelect } from "@/components/admin/fields";
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
import { composeInvalid } from "@/lib/compose";
import { errorMessage } from "@/lib/errors";
import { composeImageServices, imageSetTarget } from "@/lib/newproject";
import { useApi } from "@/lib/rpc-context";
import type { PolicyViolation } from "@/lib/types";
import { Rejection } from "@/pages/admin/ProjectCompose";

/** Admin: release a new image tag (or reference) of a compose project as a new compose version. */
export function UpdateImageButton({
  slug,
  ready,
  onDeploy,
}: {
  slug: string;
  ready: boolean;
  /** Called when a deploy was queued, to follow it in the Deploys tab. */
  onDeploy: () => void;
}) {
  const [open, setOpen] = React.useState(false);
  return (
    <>
      <Button variant="outline" onClick={() => setOpen(true)}>
        <PackageOpen /> Update image
      </Button>
      {open && <UpdateImageDialog slug={slug} ready={ready} onClose={() => setOpen(false)} onDeploy={onDeploy} />}
    </>
  );
}

function UpdateImageDialog({
  slug,
  ready,
  onClose,
  onDeploy,
}: {
  slug: string;
  ready: boolean;
  onClose: () => void;
  onDeploy: () => void;
}) {
  const api = useApi();
  const qc = useQueryClient();
  const current = useQuery({
    queryKey: ["compose", slug, "current"],
    queryFn: () => api.projects.composeGet({ slug }),
    retry: false,
  });
  const services = React.useMemo(() => composeImageServices(current.data?.content ?? ""), [current.data]);
  const [service, setService] = React.useState("");
  const [target, setTarget] = React.useState("");
  const [note, setNote] = React.useState("");
  const [deploy, setDeploy] = React.useState(ready);
  const [error, setError] = React.useState<string | null>(null);
  const [rejected, setRejected] = React.useState<{ detail: string; violations: PolicyViolation[] } | null>(null);
  const chosen = service || services[0]?.name || "";
  const currentImage = services.find((s) => s.name === chosen)?.image;

  const m = useMutation({
    mutationFn: (t: { image: string } | { tag: string }) =>
      api.projects.imageSet({
        slug,
        ...t,
        service: chosen || undefined,
        note: note.trim() || undefined,
        deploy: deploy || undefined,
      }),
    onMutate: () => {
      setError(null);
      setRejected(null);
    },
    onSuccess: (r) => {
      toast.success(`Saved v${r.version}`, {
        description: r.deploy ? `${r.image}, deploy #${r.deploy.id} queued` : r.image,
      });
      void qc.invalidateQueries({ queryKey: ["compose", slug] });
      void qc.invalidateQueries({ queryKey: ["project", slug] });
      void qc.invalidateQueries({ queryKey: ["projects"] });
      void qc.invalidateQueries({ queryKey: ["deploys", slug] });
      onClose();
      if (r.deploy) onDeploy();
    },
    onError: (e) => {
      const invalid = composeInvalid(e);
      if (invalid) setRejected(invalid);
      else setError(errorMessage(e));
    },
  });

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const t = imageSetTarget(target);
    if ("error" in t) {
      setError(t.error);
      setRejected(null);
      return;
    }
    m.mutate(t);
  };

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Update image</DialogTitle>
          <DialogDescription>Stores a new compose version with the new image.</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          {services.length > 1 && (
            <Field id="ui-service" label="Service">
              <NativeSelect id="ui-service" value={chosen} onChange={(e) => setService(e.target.value)}>
                {services.map((s) => (
                  <option key={s.name} value={s.name}>
                    {s.name}
                  </option>
                ))}
              </NativeSelect>
            </Field>
          )}
          <Field
            id="ui-target"
            label="New tag or image reference"
            hint={
              <>
                A bare tag such as 1.4.3 replaces the tag of the current image
                {currentImage ? <span className="font-mono"> ({currentImage})</span> : null}; a full reference
                (with <span className="font-mono">/</span> or <span className="font-mono">@</span>) replaces it entirely.
              </>
            }
          >
            <Input
              id="ui-target"
              value={target}
              onChange={(e) => setTarget(e.target.value)}
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              className="font-mono"
            />
          </Field>
          <Field id="ui-note" label="Note (optional)">
            <Input id="ui-note" value={note} onChange={(e) => setNote(e.target.value)} maxLength={200} autoComplete="off" />
          </Field>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={deploy} disabled={!ready} onChange={(e) => setDeploy(e.target.checked)} />
            Deploy now
          </label>
          {error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          {rejected && (
            <Rejection title="The image update was rejected" detail={rejected.detail} violations={rejected.violations} />
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={m.isPending}>
              {m.isPending ? "Saving…" : deploy ? "Save and deploy" : "Save new version"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
