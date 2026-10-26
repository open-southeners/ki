import { NativeSelect } from "@/components/admin/fields";
import { Badge } from "@/components/ui/badge";
import type { Role } from "@/lib/types";

export const ROLES: Role[] = ["viewer", "operator", "admin"];

export function RoleBadge({ role }: { role: Role }) {
  return <Badge variant={role === "admin" ? "default" : role === "operator" ? "secondary" : "outline"}>{role}</Badge>;
}

export function RoleSelect({
  id,
  value,
  onChange,
  disabled,
  label,
}: {
  id?: string;
  value: Role;
  onChange: (r: Role) => void;
  disabled?: boolean;
  label?: string;
}) {
  return (
    <NativeSelect
      id={id}
      aria-label={label}
      value={value}
      disabled={disabled}
      onChange={(e) => onChange(e.target.value as Role)}
    >
      {ROLES.map((r) => (
        <option key={r} value={r}>
          {r}
        </option>
      ))}
    </NativeSelect>
  );
}
