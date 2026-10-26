import { useQuery } from "@tanstack/react-query";
import { Eye, EyeOff, Loader2, Smartphone } from "lucide-react";
import QRCode from "qrcode";
import * as React from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { AuthLayout, FormError } from "@/components/auth/AuthLayout";
import { CopyButton } from "@/components/copy-button";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  AuthError,
  PASSWORD_MAX,
  PASSWORD_MIN,
  describeAuthError,
  setupBegin,
  setupFinish,
  type SetupInfo,
} from "@/lib/auth";
import { useAuth } from "@/lib/auth-context";
import { useRetryCountdown } from "@/lib/use-retry-countdown";

/** Groups a base32 secret for reading aloud / typing: "ABCD EFGH ...". */
export function groupSecret(s: string): string {
  return s.replace(/\s+/g, "").replace(/(.{4})/g, "$1 ").trim();
}

/** otpauth links only; anything else is never put in an href. */
export function isOtpauth(uri: string): boolean {
  return uri.startsWith("otpauth://");
}

export default function Setup() {
  const loc = useLocation();
  const token = React.useMemo(() => loc.hash.replace(/^#/, "").trim(), [loc.hash]);

  // One begin call per token: every call replaces the TOTP secret on the server.
  const begin = useQuery({
    queryKey: ["setup-begin", token],
    queryFn: () => setupBegin(token),
    enabled: token !== "",
    staleTime: Infinity,
    gcTime: Infinity,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    refetchOnMount: false,
  });

  if (token === "") {
    return (
      <AuthLayout title="Account setup">
        <FormError>
          This page needs a setup link. Ask an administrator for one (it looks like /setup#…), or{" "}
          <Link to="/login" className="underline">
            sign in
          </Link>{" "}
          if you already have an account.
        </FormError>
      </AuthLayout>
    );
  }
  if (begin.isPending) {
    return (
      <AuthLayout title="Account setup">
        <p className="label-mono animate-ki-pulse" role="status">
          Checking your setup link
        </p>
      </AuthLayout>
    );
  }
  if (begin.isError) {
    return (
      <AuthLayout title="Account setup">
        <div className="grid gap-4">
          <FormError>{describeAuthError(begin.error)}</FormError>
          <p className="text-sm text-muted-foreground">
            Setup links work once and expire after 30 minutes. Ask an administrator for a new one.
          </p>
          <Button variant="outline" asChild>
            <Link to="/login">Go to sign in</Link>
          </Button>
        </div>
      </AuthLayout>
    );
  }
  return <SetupForm token={token} info={begin.data} />;
}

function SetupForm({ token, info }: { token: string; info: SetupInfo }) {
  const { setUser } = useAuth();
  const nav = useNavigate();
  const [password, setPassword] = React.useState("");
  const [confirm, setConfirm] = React.useState("");
  const [code, setCode] = React.useState("");
  const [reveal, setReveal] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);
  const wait = useRetryCountdown();
  const qr = useQrDataUrl(info.otpauth_uri);

  const tooShort = password.length > 0 && password.length < PASSWORD_MIN;
  const mismatch = confirm.length > 0 && password !== confirm;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy || wait.remaining > 0) return;
    if (password.length < PASSWORD_MIN) return setError(`Password must be at least ${PASSWORD_MIN} characters.`);
    if (password !== confirm) return setError("The two passwords do not match.");
    setBusy(true);
    setError(null);
    try {
      const s = await setupFinish(token, password, code.trim());
      setUser(s);
      nav("/", { replace: true });
    } catch (err) {
      setError(describeAuthError(err));
      if (err instanceof AuthError && (err.status === 429 || err.status === 503)) wait.start(err.retryAfter);
      setCode("");
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthLayout
      wide
      title="Set up your account"
      subtitle={
        <span className="flex flex-wrap items-center gap-2">
          Welcome, <strong className="font-mono text-foreground">{info.username}</strong>
          <Badge variant="secondary">{info.role}</Badge>
          Add the authenticator, choose a password, then confirm with a code.
        </span>
      }
    >
      <form onSubmit={(e) => void submit(e)} className="grid gap-6">
        <section aria-labelledby="step-totp" className="grid gap-3">
          <h2 id="step-totp" className="label-mono flex items-center gap-2">
            <Smartphone className="size-3.5" aria-hidden /> 1 · Authenticator
          </h2>
          <div className="grid gap-4 sm:grid-cols-[auto_1fr] sm:items-center">
            <div className="grid size-[200px] place-items-center rounded-md border bg-white p-2">
              {qr ? (
                <img src={qr} alt="QR code to add this account to an authenticator app" width={184} height={184} />
              ) : (
                <span className="label-mono animate-ki-pulse !text-neutral-500">Rendering</span>
              )}
            </div>
            <div className="grid min-w-0 gap-2">
              <p className="text-sm text-muted-foreground">
                Scan the code with an authenticator app, or enter the key by hand.
              </p>
              <p
                className="break-words rounded-md border bg-muted px-3 py-2 font-mono text-[13px] tracking-wider"
                aria-label="Authenticator key"
              >
                {groupSecret(info.totp_secret)}
              </p>
              <div className="flex flex-wrap items-center gap-2">
                <CopyButton value={info.totp_secret} label="authenticator key" />
                {isOtpauth(info.otpauth_uri) && (
                  <Button variant="outline" size="sm" asChild>
                    <a href={info.otpauth_uri}>Open in authenticator</a>
                  </Button>
                )}
              </div>
            </div>
          </div>
        </section>

        <section aria-labelledby="step-pw" className="grid gap-4">
          <h2 id="step-pw" className="label-mono">
            2 · Password
          </h2>
          {error && <FormError id="setup-error">{error}</FormError>}
          <div className="grid items-start gap-4 sm:grid-cols-2">
            <div className="grid gap-1.5">
              <Label htmlFor="new-password">New password</Label>
              <div className="relative">
                <Input
                  id="new-password"
                  type={reveal ? "text" : "password"}
                  autoComplete="new-password"
                  required
                  minLength={PASSWORD_MIN}
                  maxLength={PASSWORD_MAX}
                  className="pr-10"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  aria-invalid={tooShort || undefined}
                  aria-describedby="pw-hint"
                />
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className="absolute right-0 top-0 size-9"
                  onClick={() => setReveal((v) => !v)}
                  aria-label={reveal ? "Hide passwords" : "Show passwords"}
                  aria-pressed={reveal}
                >
                  {reveal ? <EyeOff /> : <Eye />}
                </Button>
              </div>
              <p id="pw-hint" className={`text-xs ${tooShort ? "text-destructive" : "text-muted-foreground"}`}>
                At least {PASSWORD_MIN} characters, and not your username.
              </p>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="confirm-password">Repeat password</Label>
              <Input
                id="confirm-password"
                type={reveal ? "text" : "password"}
                autoComplete="new-password"
                required
                maxLength={PASSWORD_MAX}
                value={confirm}
                onChange={(e) => setConfirm(e.target.value)}
                aria-invalid={mismatch || undefined}
                aria-describedby={mismatch ? "pw-mismatch" : undefined}
              />
              {mismatch && (
                <p id="pw-mismatch" className="text-xs text-destructive">
                  The passwords do not match.
                </p>
              )}
            </div>
          </div>
        </section>

        <section aria-labelledby="step-code" className="grid gap-3">
          <h2 id="step-code" className="label-mono">
            3 · Confirm
          </h2>
          <div className="grid gap-1.5 sm:max-w-[16rem]">
            <Label htmlFor="setup-code">Code from the app</Label>
            <Input
              id="setup-code"
              inputMode="numeric"
              autoComplete="one-time-code"
              pattern="[0-9]{6}"
              maxLength={6}
              placeholder="000000"
              required
              className="font-mono text-base tracking-[0.35em]"
              value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
            />
          </div>
        </section>

        <Button type="submit" size="lg" disabled={busy || wait.remaining > 0} className="w-full">
          {busy && <Loader2 className="animate-spin" aria-hidden />}
          {wait.remaining > 0 ? `Try again in ${wait.remaining}s` : busy ? "Creating account" : "Activate and sign in"}
        </Button>
      </form>
    </AuthLayout>
  );
}

/** Renders the otpauth URI to a data: URL (the CSP allows data: images for exactly this). */
function useQrDataUrl(uri: string): string | null {
  const [url, setUrl] = React.useState<string | null>(null);
  React.useEffect(() => {
    let live = true;
    QRCode.toDataURL(uri, { margin: 0, width: 368, errorCorrectionLevel: "M", color: { dark: "#000000", light: "#ffffff" } })
      .then((u) => live && setUrl(u))
      .catch(() => live && setUrl(null));
    return () => {
      live = false;
    };
  }, [uri]);
  return url;
}
