import { Route, Routes } from "react-router-dom";
import { AppShell } from "@/components/shell/AppShell";
import { RequireAuth, RequireRole } from "@/lib/auth-context";
import Login from "@/pages/Login";
import NotFound from "@/pages/NotFound";
import Overview from "@/pages/Overview";
import Project from "@/pages/Project";
import Projects from "@/pages/Projects";
import Setup from "@/pages/Setup";
import Audit from "@/pages/admin/Audit";
import Edge from "@/pages/admin/Edge";
import Tokens from "@/pages/admin/Tokens";
import Users from "@/pages/admin/Users";

/**
 * Every route of the panel. Pages live one per file under src/pages; later
 * work edits those files, not this table.
 */
export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/setup" element={<Setup />} />
      <Route element={<RequireAuth />}>
        <Route element={<AppShell />}>
          <Route index element={<Overview />} />
          <Route path="projects" element={<Projects />} />
          <Route path="projects/:slug" element={<Project />} />
          <Route element={<RequireRole role="admin" />}>
            <Route path="admin/users" element={<Users />} />
            <Route path="admin/tokens" element={<Tokens />} />
            <Route path="admin/audit" element={<Audit />} />
            <Route path="admin/edge" element={<Edge />} />
          </Route>
          <Route path="*" element={<NotFound />} />
        </Route>
      </Route>
    </Routes>
  );
}
