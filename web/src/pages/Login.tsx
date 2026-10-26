import { Loader2 } from "lucide-react";
import * as React from "react";
import { Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { AuthLayout, FormError } from "@/components/auth/AuthLayout";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { AuthError, describeAuthError, login } from "@/lib/auth";
import { FullScreenMessage, useAuth } from "@/lib/auth-context";
import { useRetryCountdown } from "@/lib/use-retry-countdown";

/** Only same-origin absolute paths are honoured as a post-login destination. */
export function safeNext(next: string | null): string {
  return next && next.startsWith("/") && !next.startsWith("//") && !next.startsWith("/\\") ? next : "/";
}

export default function Login() {
  const { user, setUser } = useAuth();
  const nav = useNavigate();
  const [params] = useSearchParams();
  const next = safeNext(params.get("next"));
  const [username, setUsername] = React.useState("");
  const [password, setPassword] = React.useState("");
  const [code, setCode] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);
  const wait = useRetryCountdown();

  if (user === undefined) return <FullScreenMessage>Checking session</FullScreenMessage>;
  if (user) return <Navigate to={next} replace />;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy || wait.remaining > 0) return;
    setBusy(true);
    setError(null);
    try {
      const s = await login(username.trim(), password, code.trim());
      setUser(s);
      nav(next, { replace: true });
    } catch (err) {
      setError(describeAuthError(err));
      if (err instanceof AuthError && (err.status === 429 || err.status === 503)) wait.start(err.retryAfter);
      setCode("");
    } finally {
      setBusy(false);
    }
  };

  const blocked = busy || wait.remaining > 0;
  return (
    <AuthLayout title="Sign in" subtitle="Use your password and the current code from your authenticator app.">
      <form onSubmit={(e) => void submit(e)} className="grid gap-4" noValidate={false}>
        {error && <FormError id="login-error">{error}</FormError>}
        <div className="grid gap-1.5">
          <Label htmlFor="username">Username</Label>
          <Input
            id="username"
            name="username"
            autoComplete="username"
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            required
            autoFocus
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            aria-invalid={error ? true : undefined}
            aria-describedby={error ? "login-error" : undefined}
          />
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="password">Password</Label>
          <Input
            id="password"
            name="password"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            aria-invalid={error ? true : undefined}
          />
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="code">Authenticator code</Label>
          <Input
            id="code"
            name="code"
            inputMode="numeric"
            autoComplete="one-time-code"
            pattern="[0-9]{6}"
            maxLength={6}
            placeholder="000000"
            required
            className="font-mono text-base tracking-[0.35em]"
            value={code}
            onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
            aria-invalid={error ? true : undefined}
          />
        </div>
        <Button type="submit" disabled={blocked} className="mt-1 w-full">
          {busy && <Loader2 className="animate-spin" aria-hidden />}
          {wait.remaining > 0 ? `Try again in ${wait.remaining}s` : busy ? "Signing in" : "Sign in"}
        </Button>
      </form>
    </AuthLayout>
  );
}
