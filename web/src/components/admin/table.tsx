import * as React from "react";
import { cn } from "@/lib/utils";

/** Hairline data table that scrolls sideways on narrow screens instead of squeezing columns. */
export function DataTable({ className, ...props }: React.ComponentProps<"table">) {
  return (
    <div className="overflow-x-auto rounded-md border bg-card">
      <table className={cn("w-full min-w-max border-collapse text-sm", className)} {...props} />
    </div>
  );
}

export function Th({ className, ...props }: React.ComponentProps<"th">) {
  return <th scope="col" className={cn("label-mono border-b px-3 py-2 text-left font-medium", className)} {...props} />;
}

export function Tr({ className, ...props }: React.ComponentProps<"tr">) {
  return <tr className={cn("border-b last:border-b-0", className)} {...props} />;
}

export function Td({ className, ...props }: React.ComponentProps<"td">) {
  return <td className={cn("px-3 py-2 align-middle", className)} {...props} />;
}

/** Machine value (name, id, address) in the mono face. */
export function Mono({ className, ...props }: React.ComponentProps<"span">) {
  return <span className={cn("font-mono text-[13px]", className)} {...props} />;
}
