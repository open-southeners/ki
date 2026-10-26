import { Toaster as Sonner, type ToasterProps } from "sonner";
import { useTheme } from "@/lib/theme";

export function Toaster(props: ToasterProps) {
  const { resolved } = useTheme();
  return (
    <Sonner
      theme={resolved}
      position="bottom-right"
      closeButton
      toastOptions={{
        classNames: {
          toast:
            "!rounded-md !border !border-border !bg-popover !text-popover-foreground !font-sans !shadow-none",
          description: "!text-muted-foreground",
        },
      }}
      {...props}
    />
  );
}
