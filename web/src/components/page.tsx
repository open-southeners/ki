import * as React from "react";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";

/** Page title row: a mono eyebrow, the heading, an optional description and actions on the right. */
export function PageHeader({
  eyebrow,
  title,
  description,
  actions,
  className,
}: {
  eyebrow?: string;
  title: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("mb-6 flex flex-wrap items-end justify-between gap-4 border-b pb-5", className)}>
      <div className="min-w-0">
        {eyebrow && <p className="label-mono mb-1.5">{eyebrow}</p>}
        <h1 className="truncate text-2xl font-semibold tracking-tight">{title}</h1>
        {description && <p className="mt-1 max-w-prose text-sm text-muted-foreground">{description}</p>}
      </div>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </div>
  );
}

/** Stand-in body for pages that a later unit fills in. */
export function PlaceholderCard({ title, children }: { title: string; children?: React.ReactNode }) {
  return (
    <Card className="border-dashed">
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        <CardDescription>{children ?? "This section is not built yet."}</CardDescription>
      </CardHeader>
      <CardContent />
    </Card>
  );
}
