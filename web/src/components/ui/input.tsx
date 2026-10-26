import * as React from "react";
import { cn } from "@/lib/utils";

export const Input = React.forwardRef<HTMLInputElement, React.ComponentProps<"input">>(
  ({ className, type, ...props }, ref) => (
    <input
      ref={ref}
      type={type}
      className={cn(
        "flex h-9 w-full rounded-md border border-input bg-card px-3 py-1 text-sm transition-colors placeholder:text-muted-foreground/70 disabled:cursor-not-allowed disabled:opacity-50 aria-[invalid=true]:border-destructive",
        className,
      )}
      {...props}
    />
  ),
);
Input.displayName = "Input";
