import type { UseQueryResult } from "@tanstack/react-query";
import { AlertTriangle } from "lucide-react";
import * as React from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { errorMessage } from "@/lib/errors";

/** Loading skeleton, error with retry, or the children once data is there. */
export function QueryBoundary<T>({
  query,
  what,
  children,
}: {
  query: Pick<UseQueryResult<T>, "isPending" | "isError" | "error" | "data" | "refetch" | "isFetching">;
  what: string;
  children: (data: T) => React.ReactNode;
}) {
  if (query.isPending) {
    return (
      <div className="grid gap-2" role="status" aria-label={`Loading ${what}`}>
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-9 w-full" />
      </div>
    );
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertTriangle />
        <div className="grid flex-1 gap-2">
          <div>
            <AlertTitle>Could not load {what}</AlertTitle>
            <AlertDescription>{errorMessage(query.error)}</AlertDescription>
          </div>
          <div>
            <Button size="sm" variant="outline" disabled={query.isFetching} onClick={() => void query.refetch()}>
              Retry
            </Button>
          </div>
        </div>
      </Alert>
    );
  }
  return <>{children(query.data as T)}</>;
}

export function EmptyState({ children }: { children: React.ReactNode }) {
  return (
    <div className="rounded-md border border-dashed px-4 py-8 text-center">
      <p className="label-mono">{children}</p>
    </div>
  );
}

/** Mono caption + content block used to group a page into labelled sections. */
export function Section({
  title,
  description,
  actions,
  children,
}: {
  title: string;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <section className="mb-8">
      <div className="mb-3 flex flex-wrap items-end justify-between gap-2">
        <div>
          <h2 className="label-mono">{title}</h2>
          {description && <p className="mt-1 max-w-prose text-sm text-muted-foreground">{description}</p>}
        </div>
        {actions && <div className="flex items-center gap-2">{actions}</div>}
      </div>
      {children}
    </section>
  );
}
