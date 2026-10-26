import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Ban, Plus } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { ConfirmDialog } from "@/components/admin/confirm";
import { Field, NativeSelect } from "@/components/admin/fields";
import { RoleBadge, RoleSelect } from "@/components/admin/role";
import { SecretDialog } from "@/components/admin/secret-dialog";
import { EmptyState, QueryBoundary } from "@/components/admin/state";
import { DataTable, Mono, Td, Th, Tr } from "@/components/admin/table";
import { PageHeader } from "@/components/page";
import { Alert, AlertDescription } from "@/components/ui/alert";
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
import { EXPIRY_PRESETS, expiryDuration } from "@/lib/admin";
import { errorMessage } from "@/lib/errors";
import { formatDateTime, formatRelative } from "@/lib/format";
import { useApi } from "@/lib/rpc-context";
import type { CreatedToken, Role, TokenInfo } from "@/lib/types";

const KEY = ["admin", "tokens"] as const;

function isExpired(t: TokenInfo): boolean {
  return !!t.expires_at && Date.parse(t.expires_at) < Date.now();
}

/** Admin: API tokens. Create (shown once), revoke. */
export default function Tokens() {
  const api = useApi();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: KEY, queryFn: () => api.tokens.list() });
  const [creating, setCreating] = React.useState(false);
  const [created, setCreated] = React.useState<CreatedToken | null>(null);
  const [revoking, setRevoking] = React.useState<TokenInfo | null>(null);
  const refresh = () => qc.invalidateQueries({ queryKey: KEY });

  return (
    <>
      <PageHeader
        eyebrow="Admin"
        title="API tokens"
        description="Bearer tokens for scripts and the CLI. A token acts with the role it was created with."
        actions={
          <Button onClick={() => setCreating(true)}>
            <Plus /> New token
          </Button>
        }
      />
      <QueryBoundary query={q} what="tokens">
        {(tokens) =>
          tokens.length === 0 ? (
            <EmptyState>No API tokens</EmptyState>
          ) : (
            <DataTable>
              <thead>
                <tr>
                  <Th>Name</Th>
                  <Th>Role</Th>
                  <Th>Created</Th>
                  <Th>Created by</Th>
                  <Th>Expires</Th>
                  <Th>Last used</Th>
                  <Th>State</Th>
                  <Th className="text-right">Actions</Th>
                </tr>
              </thead>
              <tbody>
                {tokens.map((t) => (
                  <Tr key={t.name} className={t.revoked ? "text-muted-foreground" : undefined}>
                    <Td>
                      <Mono className="font-medium">{t.name}</Mono>
                    </Td>
                    <Td>
                      <RoleBadge role={t.role} />
                    </Td>
                    <Td className="whitespace-nowrap" title={formatDateTime(t.created_at)}>
                      {formatRelative(t.created_at)}
                    </Td>
                    <Td>
                      <Mono>{t.created_by}</Mono>
                    </Td>
                    <Td className="whitespace-nowrap" title={formatDateTime(t.expires_at)}>
                      {t.expires_at ? formatRelative(t.expires_at) : "never"}
                    </Td>
                    <Td className="whitespace-nowrap" title={formatDateTime(t.last_used_at)}>
                      {t.last_used_at ? formatRelative(t.last_used_at) : "never"}
                    </Td>
                    <Td>
                      {t.revoked ? (
                        <Badge variant="destructive">revoked</Badge>
                      ) : isExpired(t) ? (
                        <Badge variant="warning">expired</Badge>
                      ) : (
                        <Badge variant="success">active</Badge>
                      )}
                    </Td>
                    <Td>
                      <div className="flex justify-end">
                        <Button size="sm" variant="outline" disabled={t.revoked} onClick={() => setRevoking(t)}>
                          <Ban /> Revoke<span className="sr-only"> {t.name}</span>
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

      <CreateTokenDialog
        open={creating}
        onOpenChange={setCreating}
        onCreated={(t) => {
          setCreating(false);
          setCreated(t);
          toast.success(`Created token ${t.name}`);
          void refresh();
        }}
      />

      {created && (
        <SecretDialog
          open
          onOpenChange={(o) => !o && setCreated(null)}
          title={`Token ${created.name}`}
          description={
            <>
              Role <span className="font-mono">{created.role}</span>
              {created.expires_at ? <>, expires {formatDateTime(created.expires_at)}.</> : <>, no expiry.</>}
            </>
          }
          warning="Copy it now. The token is shown only once and cannot be recovered; create a new one if it is lost."
          valueLabel="API token"
          value={created.token}
        />
      )}

      <ConfirmDialog
        open={revoking !== null}
        onOpenChange={(o) => !o && setRevoking(null)}
        title={`Revoke ${revoking?.name ?? ""}?`}
        description="Anything using this token stops working immediately. This cannot be undone."
        confirmLabel="Revoke token"
        onConfirm={async () => {
          if (!revoking) return;
          await api.tokens.revoke({ name: revoking.name });
          toast.success(`Revoked ${revoking.name}`);
          void refresh();
        }}
      />
    </>
  );
}

function CreateTokenDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onCreated: (t: CreatedToken) => void;
}) {
  const api = useApi();
  const [name, setName] = React.useState("");
  const [role, setRole] = React.useState<Role>("viewer");
  const [expiry, setExpiry] = React.useState("90");
  const m = useMutation({
    mutationFn: () => api.tokens.create({ name: name.trim(), role, expires_in: expiryDuration(expiry) }),
    onSuccess: (t) => {
      setName("");
      setRole("viewer");
      setExpiry("90");
      onCreated(t);
    },
  });
  React.useEffect(() => {
    if (open) m.reset();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            m.mutate();
          }}
        >
          <DialogHeader>
            <DialogTitle>New API token</DialogTitle>
            <DialogDescription>The secret is shown once after creation.</DialogDescription>
          </DialogHeader>
          <Field id="tok-name" label="Name" hint="What it is used for, e.g. ci-deploy.">
            <Input
              id="tok-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              required
              className="font-mono"
            />
          </Field>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field id="tok-role" label="Role">
              <RoleSelect id="tok-role" value={role} onChange={setRole} />
            </Field>
            <Field id="tok-exp" label="Expires">
              <NativeSelect id="tok-exp" value={expiry} onChange={(e) => setExpiry(e.target.value)}>
                {EXPIRY_PRESETS.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.label}
                  </option>
                ))}
              </NativeSelect>
            </Field>
          </div>
          {m.isError && (
            <Alert variant="destructive">
              <AlertDescription>{errorMessage(m.error)}</AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={m.isPending || name.trim() === ""}>
              {m.isPending ? "Creating…" : "Create token"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
