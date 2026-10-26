import * as React from "react";
import { cn } from "@/lib/utils";

/** Hairline table inside a horizontally scrolling wrapper (keeps 400 px layouts usable). */
export const Table = React.forwardRef<HTMLTableElement, React.ComponentProps<"table">>(({ className, ...props }, ref) => (
  <div className="relative w-full overflow-x-auto">
    <table ref={ref} className={cn("w-full caption-bottom text-sm", className)} {...props} />
  </div>
));
Table.displayName = "Table";

export function TableHeader({ className, ...props }: React.ComponentProps<"thead">) {
  return <thead className={cn("border-b [&_tr]:border-b-0", className)} {...props} />;
}

export function TableBody({ className, ...props }: React.ComponentProps<"tbody">) {
  return <tbody className={cn("[&_tr:last-child]:border-0", className)} {...props} />;
}

export function TableRow({ className, ...props }: React.ComponentProps<"tr">) {
  return <tr className={cn("border-b transition-colors hover:bg-muted/50 data-[state=selected]:bg-muted", className)} {...props} />;
}

export function TableHead({ className, ...props }: React.ComponentProps<"th">) {
  return (
    <th
      className={cn("label-mono h-9 whitespace-nowrap px-3 text-left align-middle font-medium first:pl-5 last:pr-5", className)}
      {...props}
    />
  );
}

export function TableCell({ className, ...props }: React.ComponentProps<"td">) {
  return <td className={cn("px-3 py-2.5 align-middle first:pl-5 last:pr-5", className)} {...props} />;
}

export function TableCaption({ className, ...props }: React.ComponentProps<"caption">) {
  return <caption className={cn("mt-3 text-sm text-muted-foreground", className)} {...props} />;
}
