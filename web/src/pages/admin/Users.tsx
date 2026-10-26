import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound, Plus, Trash2, UserCheck, UserX } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { ConfirmDialog } from "@/components/admin/confirm";
import { Field } from "@/components/admin/fields";
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
import { losesAdmin } from "@/lib/admin";
import { useAuth } from "@/lib/auth-context";
import { useApi } from "@/lib/rpc-context";
import { errorMessage, toastError } from "@/lib/errors";
import { formatDateTime, formatRelative } from "@/lib/format";
import type { Role, SetupLink, User } from "@/lib/types";

const KEY = ["admin", "users"] as const;

const STATUS_VARIANT = { active: "success", pending: "warning", disabled: "destructive" } as const;

/** Admin: panel accounts. Create (setup link), role, disable/enable, reset, delete. */
export default function Users() {
  const api = useApi();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: KEY, queryFn: () => api.users.list() });
  const [creating, setCreating] = React.useState(false);
  const [link, setLink] = React.useState<{ link: SetupLink; username: string } | null>(null);
  const [deleting, setDeleting] = React.useState<User | null>(null);
  const [resetting, setResetting] = React.useState<User | null>(null);
  const [roleChange, setRoleChange] = React.useState<{ user: User; role: Role } | null>(null);
  const [disabling, setDisabling] = React.useState<User | null>(null);
  const { user: me } = useAuth();
  const isMe = (u: User | null) => !!u && u.username === me?.username;

  const refresh = () => qc.invalidateQueries({ queryKey: KEY });

  const update = useMutation({
    mutationFn: (p: { username: string; role?: Role; disabled?: boolean }) => api.users.update(p),
    onSuccess: (u, p) => {
      toast.success(
        p.role !== undefined
          ? `${u.username} is now ${u.role}`
          : p.disabled
            ? `${u.username} disabled`
            : `${u.username} enabled`,
      );
      void refresh();
    },
    onError: (e, p) => {
      toastError(`Could not update ${p.username}`, e);
      void refresh(); // put the role select back to the stored value
    },
  });

  return (
    <>
      <PageHeader
        eyebrow="Admin"
        title="Users"
        description="Panel accounts and their roles. New accounts start with a one-time setup link."
        actions={
          <Button onClick={() => setCreating(true)}>
            <Plus /> New user
          </Button>
        }
      />
      <QueryBoundary query={q} what="users">
        {(users) =>
          users.length === 0 ? (
            <EmptyState>No users</EmptyState>
          ) : (
            <DataTable>
              <thead>
                <tr>
                  <Th>Username</Th>
                  <Th>Role</Th>
                  <Th>Status</Th>
                  <Th>TOTP</Th>
                  <Th>Created</Th>
                  <Th>Last login</Th>
                  <Th className="text-right">Actions</Th>
                </tr>
              </thead>
              <tbody>
                {users.map((u) => (
                  <Tr key={u.username}>
                    <Td>
                      <Mono className="font-medium">{u.username}</Mono>
                    </Td>
                    <Td>
                      <div className="flex items-center gap-2">
                        <RoleBadge role={u.role} />
                        <RoleSelect
                          label={`Role of ${u.username}`}
                          value={u.role}
                          disabled={update.isPending}
                          onChange={(role) => role !== u.role && setRoleChange({ user: u, role })}
                        />
                      </div>
                    </Td>
                    <Td>
                      <Badge variant={STATUS_VARIANT[u.status]}>{u.status}</Badge>
                    </Td>
                    <Td>
                      <Badge variant={u.totp_enrolled ? "success" : "outline"}>
                        {u.totp_enrolled ? "enrolled" : "not enrolled"}
                      </Badge>
                    </Td>
                    <Td className="whitespace-nowrap text-muted-foreground" title={formatDateTime(u.created_at)}>
                      {formatRelative(u.created_at)}
                    </Td>
                    <Td className="whitespace-nowrap text-muted-foreground" title={formatDateTime(u.last_login_at)}>
                      {u.last_login_at ? formatRelative(u.last_login_at) : "never"}
                    </Td>
                    <Td>
                      <div className="flex justify-end gap-1.5">
                        {u.status !== "pending" && (
                          <Button
                            size="sm"
                            variant="outline"
                            disabled={update.isPending}
                            onClick={() =>
                              u.status === "disabled" || !isMe(u)
                                ? update.mutate({ username: u.username, disabled: u.status !== "disabled" })
                                : setDisabling(u)
                            }
                          >
                            {u.status === "disabled" ? <UserCheck /> : <UserX />}
                            {u.status === "disabled" ? "Enable" : "Disable"}
                            <span className="sr-only"> {u.username}</span>
                          </Button>
                        )}
                        <Button size="sm" variant="outline" onClick={() => setResetting(u)}>
                          <KeyRound /> Reset<span className="sr-only"> {u.username}</span>
                        </Button>
                        <Button size="sm" variant="outline" onClick={() => setDeleting(u)}>
                          <Trash2 /> Delete<span className="sr-only"> {u.username}</span>
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

      <CreateUserDialog
        open={creating}
        onOpenChange={setCreating}
        onCreated={(l) => {
          setCreating(false);
          setLink({ link: l, username: l.user.username });
          toast.success(`Created ${l.user.username}`);
          void refresh();
        }}
      />

      {link && (
        <SetupLinkDialog
          link={link.link}
          username={link.username}
          open
          onOpenChange={(o) => !o && setLink(null)}
        />
      )}

      <ConfirmDialog
        open={roleChange !== null}
        onOpenChange={(o) => !o && setRoleChange(null)}
        title={`Change role of ${roleChange?.user.username ?? ""}?`}
        description={
          roleChange && (
            <div className="grid gap-2">
              <p>
                Change <strong>{roleChange.user.username}</strong> from{" "}
                <span className="font-mono">{roleChange.user.role}</span> to{" "}
                <span className="font-mono">{roleChange.role}</span>? Their open sessions are closed.
              </p>
              {isMe(roleChange.user) && losesAdmin(roleChange.user.role, roleChange.role) && (
                <p className="font-medium text-destructive">
                  This is your own account: you will lose access to the admin pages and be signed out of your open
                  connections.
                </p>
              )}
            </div>
          )
        }
        confirmLabel="Change role"
        onConfirm={async () => {
          if (roleChange) await update.mutateAsync({ username: roleChange.user.username, role: roleChange.role });
        }}
      />

      <ConfirmDialog
        open={disabling !== null}
        onOpenChange={(o) => !o && setDisabling(null)}
        title="Disable your own account?"
        description="This is the account you are signed in with. Your open connections are closed and you cannot sign in again until another admin enables it."
        confirmLabel="Disable my account"
        onConfirm={async () => {
          if (disabling) await update.mutateAsync({ username: disabling.username, disabled: true });
        }}
      />

      <ConfirmDialog
        open={resetting !== null}
        onOpenChange={(o) => !o && setResetting(null)}
        title={`Reset ${resetting?.username ?? ""}?`}
        description="Their password and authenticator are cleared, their sessions end, and a new setup link is issued. They cannot sign in until they use it."
        confirmLabel="Reset account"
        onConfirm={async () => {
          if (!resetting) return;
          const l = await api.users.reset({ username: resetting.username });
          toast.success(`Reset ${resetting.username}`);
          setLink({ link: l, username: resetting.username });
          void refresh();
        }}
      />

      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(o) => !o && setDeleting(null)}
        title={`Delete ${deleting?.username ?? ""}?`}
        description="The account and its sessions are removed. This cannot be undone. The last active admin cannot be deleted."
        confirmLabel="Delete user"
        onConfirm={async () => {
          if (!deleting) return;
          await api.users.delete({ username: deleting.username });
          toast.success(`Deleted ${deleting.username}`);
          void refresh();
        }}
      />
    </>
  );
}

function CreateUserDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onCreated: (l: SetupLink) => void;
}) {
  const api = useApi();
  const [username, setUsername] = React.useState("");
  const [role, setRole] = React.useState<Role>("viewer");
  const m = useMutation({
    mutationFn: () => api.users.create({ username: username.trim(), role }),
    onSuccess: (l) => {
      setUsername("");
      setRole("viewer");
      onCreated(l);
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
            <DialogTitle>New user</DialogTitle>
            <DialogDescription>You get a setup link to hand over; they choose their own password.</DialogDescription>
          </DialogHeader>
          <Field id="new-user-name" label="Username">
            <Input
              id="new-user-name"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              required
              className="font-mono"
            />
          </Field>
          <Field id="new-user-role" label="Role" hint="Viewer reads, operator also deploys, admin manages the panel.">
            <RoleSelect id="new-user-role" value={role} onChange={setRole} />
          </Field>
          {m.isError && (
            <Alert variant="destructive">
              <AlertDescription>{errorMessage(m.error)}</AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={m.isPending || username.trim() === ""}>
              {m.isPending ? "Creating…" : "Create user"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function SetupLinkDialog({
  link,
  username,
  open,
  onOpenChange,
}: {
  link: SetupLink;
  username: string;
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  return (
    <SecretDialog
      open={open}
      onOpenChange={onOpenChange}
      title={`Setup link for ${username}`}
      description={
        <>
          Valid 30 minutes and works once. Expires {formatRelative(link.expires_at)}.
        </>
      }
      warning={
        <>
          Share this link only with <strong>{username}</strong>. Anyone holding it can claim the account. It is not shown
          again.
        </>
      }
      valueLabel="Setup link"
      value={link.setup_url}
      extra={
        link.fingerprint_sha256
          ? [{ label: "Certificate fingerprint (SHA-256), to compare in the browser", value: link.fingerprint_sha256 }]
          : undefined
      }
    />
  );
}
