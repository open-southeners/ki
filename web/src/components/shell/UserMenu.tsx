import { ChevronsUpDown, LogOut, Monitor, Moon, Sun } from "lucide-react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { useAuth, useRole } from "@/lib/auth-context";
import { errorMessage } from "@/lib/errors";
import { useTheme, type ThemePref } from "@/lib/theme";

export function UserMenu() {
  const { user, signOut } = useAuth();
  const role = useRole();
  const { pref, setPref } = useTheme();
  const nav = useNavigate();

  const out = async () => {
    try {
      await signOut();
    } catch (e) {
      toast.error("Sign out failed", { description: errorMessage(e) });
    }
    nav("/login", { replace: true });
  };

  return (
    <DropdownMenu>
      <DropdownMenuTrigger className="flex w-full items-center gap-2 rounded-md border bg-card px-2.5 py-2 text-left hover:bg-accent">
        <span className="grid size-7 shrink-0 place-items-center rounded-sm bg-secondary font-mono text-xs font-medium uppercase">
          {user?.username.slice(0, 2)}
        </span>
        <span className="min-w-0 flex-1">
          <span className="block truncate text-sm font-medium leading-tight">{user?.username}</span>
          <span className="label-mono block !text-[10px] leading-tight">{role ?? user?.role}</span>
        </span>
        <ChevronsUpDown className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        <span className="sr-only">Account menu</span>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="w-56">
        <DropdownMenuLabel className="flex items-center justify-between">
          Signed in <Badge variant="secondary">{role ?? user?.role}</Badge>
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuLabel>Theme</DropdownMenuLabel>
        <DropdownMenuRadioGroup value={pref} onValueChange={(v) => setPref(v as ThemePref)}>
          <DropdownMenuRadioItem value="system">
            <Monitor /> System
          </DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="light">
            <Sun /> Light
          </DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="dark">
            <Moon /> Dark
          </DropdownMenuRadioItem>
        </DropdownMenuRadioGroup>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => void out()}>
          <LogOut /> Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
