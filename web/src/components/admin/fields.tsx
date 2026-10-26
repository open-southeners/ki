import * as React from "react";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";

const control =
  "w-full rounded-md border border-input bg-card px-3 text-sm transition-colors placeholder:text-muted-foreground/70 disabled:cursor-not-allowed disabled:opacity-50 aria-[invalid=true]:border-destructive";

export const Textarea = React.forwardRef<HTMLTextAreaElement, React.ComponentProps<"textarea">>(
  ({ className, ...props }, ref) => (
    <textarea ref={ref} className={cn(control, "min-h-24 py-2 font-mono text-[13px]", className)} {...props} />
  ),
);
Textarea.displayName = "Textarea";

/** The platform <select>, styled like Input: accessible and keyboard friendly for short lists. */
export const NativeSelect = React.forwardRef<HTMLSelectElement, React.ComponentProps<"select">>(
  ({ className, ...props }, ref) => <select ref={ref} className={cn(control, "h-9 py-1", className)} {...props} />,
);
NativeSelect.displayName = "NativeSelect";

/** Label + control + optional hint, stacked. */
export function Field({
  id,
  label,
  hint,
  children,
  className,
}: {
  id: string;
  label: string;
  hint?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("grid gap-1.5", className)}>
      <Label htmlFor={id}>{label}</Label>
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}
