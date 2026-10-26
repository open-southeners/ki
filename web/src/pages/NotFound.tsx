import { Link } from "react-router-dom";
import { PageHeader } from "@/components/page";
import { Button } from "@/components/ui/button";

export default function NotFound() {
  return (
    <>
      <PageHeader eyebrow="404" title="Page not found" description="There is nothing at this address." />
      <Button asChild variant="outline">
        <Link to="/">Back to overview</Link>
      </Button>
    </>
  );
}
