import { cva, type VariantProps } from "class-variance-authority";
import * as React from "react";
import { cn } from "@/lib/utils";

const alertVariants = cva(
  "relative flex w-full gap-3 rounded-md border px-4 py-3 text-sm [&>svg]:mt-0.5 [&>svg]:size-4 [&>svg]:shrink-0",
  {
    variants: {
      variant: {
        default: "bg-card text-card-foreground",
        destructive: "border-destructive/50 bg-destructive/5 text-destructive",
        warning: "border-warning/50 bg-warning/5 text-foreground [&>svg]:text-warning",
      },
    },
    defaultVariants: { variant: "default" },
  },
);

export function Alert({
  className,
  variant,
  ...props
}: React.ComponentProps<"div"> & VariantProps<typeof alertVariants>) {
  return <div role="alert" className={cn(alertVariants({ variant }), className)} {...props} />;
}

export function AlertTitle({ className, ...props }: React.ComponentProps<"p">) {
  return <p className={cn("font-medium leading-tight", className)} {...props} />;
}

export function AlertDescription({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("text-sm opacity-90", className)} {...props} />;
}
