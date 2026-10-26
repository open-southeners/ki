import { AlertTriangle } from "lucide-react";
import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { errorMessage } from "@/lib/errors";
import { cn } from "@/lib/utils";
import type { BadgeSpec } from "./logic";

export function StatusBadge({ spec, className }: { spec: BadgeSpec; className?: string }) {
  return (
    <Badge variant={spec.tone} className={className}>
      {spec.label}
    </Badge>
  );
}

/** A mono value (sha, id, ref) that truncates instead of widening the layout. */
export function Mono({ className, ...props }: React.ComponentProps<"span">) {
  return <span className={cn("font-mono text-[13px]", className)} {...props} />;
}

export function EmptyState({
  title,
  children,
  className,
}: {
  title: string;
  children?: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("flex flex-col items-center gap-1.5 px-5 py-10 text-center", className)}>
      <p className="label-mono">{title}</p>
      {children && <p className="max-w-prose text-sm text-muted-foreground">{children}</p>}
    </div>
  );
}

export function ErrorState({
  title = "Could not load",
  error,
  onRetry,
  className,
}: {
  title?: string;
  error: unknown;
  onRetry?: () => void;
  className?: string;
}) {
  return (
    <div role="alert" className={cn("flex flex-col items-start gap-3 px-5 py-6 text-sm", className)}>
      <div className="flex items-start gap-2 text-destructive">
        <AlertTriangle className="mt-0.5 size-4 shrink-0" />
        <div>
          <p className="font-medium">{title}</p>
          <p className="text-muted-foreground">{errorMessage(error)}</p>
        </div>
      </div>
      {onRetry && (
        <Button size="sm" variant="outline" onClick={onRetry}>
          Retry
        </Button>
      )}
    </div>
  );
}

export function RowsSkeleton({ rows = 3, className }: { rows?: number; className?: string }) {
  return (
    <div className={cn("grid gap-2 p-5", className)} role="status" aria-label="Loading">
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-9 w-full" />
      ))}
    </div>
  );
}

/** Label/value tile for the host stats strip. */
export function Stat({
  label,
  value,
  sub,
  className,
}: {
  label: string;
  value: React.ReactNode;
  sub?: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("min-w-0 rounded-lg border bg-card px-4 py-3", className)}>
      <p className="label-mono">{label}</p>
      <p className="mt-1 truncate font-mono text-lg font-medium">{value}</p>
      {sub && <p className="truncate font-mono text-[11px] text-muted-foreground">{sub}</p>}
    </div>
  );
}
