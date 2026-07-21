import { FormEvent, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { Api, ApiError, googleStartURL } from "../../api";
import { Button } from "../Button";
import { AuthCard, FormError, FormNote, TextField } from "./AuthShell";

function errText(err: unknown): string {
  return err instanceof ApiError ? err.message : "Something went wrong. Try again";
}

// GoogleButton starts the OAuth flow via a top-level navigation.
function GoogleButton({ label }: { label: string }) {
  return (
    <a
      href={googleStartURL}
      className="block text-center rounded-ctl border border-line bg-surface px-4 py-2 text-text font-medium hover:border-text-2"
    >
      {label}
    </a>
  );
}

function Divider() {
  return (
    <div className="flex items-center gap-3 my-4 text-text-2 text-xs">
      <span className="h-px bg-line flex-1" /> or <span className="h-px bg-line flex-1" />
    </div>
  );
}

export function SignUp({ api }: { api: Api }) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [sent, setSent] = useState(false);
  const [busy, setBusy] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setErr(null);
    setBusy(true);
    try {
      await api.signup(email, password);
      setSent(true);
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(false);
    }
  }

  if (sent) {
    return (
      <AuthCard title="Check your email" subtitle={`We sent a verification link to ${email}.`}>
        <FormNote>
          Click the link to activate your account, then <Link className="text-green-deep" to="/login">sign in</Link>.
        </FormNote>
      </AuthCard>
    );
  }

  return (
    <AuthCard title="Create your account">
      <GoogleButton label="Continue with Google" />
      <Divider />
      <form onSubmit={onSubmit}>
        <FormError message={err} />
        <TextField label="Email" type="email" value={email} required onChange={(e) => setEmail(e.target.value)} />
        <TextField
          label="Password"
          type="password"
          value={password}
          required
          minLength={8}
          onChange={(e) => setPassword(e.target.value)}
        />
        <Button type="submit" variant="primary" className="w-full mt-1" disabled={busy}>
          {busy ? "Creating…" : "Create account"}
        </Button>
      </form>
      <FormNote>
        Already have an account? <Link className="text-green-deep" to="/login">Sign in</Link>
      </FormNote>
    </AuthCard>
  );
}

export function LogIn({ api, reload }: { api: Api; reload: () => void }) {
  const nav = useNavigate();
  const [params] = useSearchParams();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const notice = params.get("verified")
    ? "Email verified — you can sign in now."
    : params.get("verify_error")
      ? "That verification link is invalid or expired."
      : oauthNotice(params.get("oauth_error"));

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setErr(null);
    setBusy(true);
    try {
      await api.login(email, password);
      reload();
      nav("/");
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <AuthCard title="Sign in" subtitle={notice ?? undefined}>
      <GoogleButton label="Continue with Google" />
      <Divider />
      <form onSubmit={onSubmit}>
        <FormError message={err} />
        <TextField label="Email" type="email" value={email} required onChange={(e) => setEmail(e.target.value)} />
        <TextField label="Password" type="password" value={password} required onChange={(e) => setPassword(e.target.value)} />
        <div className="flex justify-end mb-3">
          <Link className="text-sm text-text-2 hover:text-text" to="/forgot">
            Forgot password?
          </Link>
        </div>
        <Button type="submit" variant="primary" className="w-full" disabled={busy}>
          {busy ? "Signing in…" : "Sign in"}
        </Button>
      </form>
      <FormNote>
        New here? <Link className="text-green-deep" to="/signup">Create an account</Link>
      </FormNote>
    </AuthCard>
  );
}

function oauthNotice(code: string | null): string | null {
  if (!code) return null;
  if (code === "verify_first")
    return "That email already has a password account — sign in with your password first, then connect Google.";
  return "Google sign-in didn't complete. Try again.";
}

export function VerifyEmailNotice() {
  return (
    <AuthCard
      title="Verify your email"
      subtitle="We've sent you a verification link. Open it to activate your account."
    >
      <FormNote>
        Already verified? <Link className="text-green-deep" to="/login">Sign in</Link>
      </FormNote>
    </AuthCard>
  );
}

export function ForgotPassword({ api }: { api: Api }) {
  const [email, setEmail] = useState("");
  const [sent, setSent] = useState(false);
  const [busy, setBusy] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      await api.forgotPassword(email);
    } finally {
      setBusy(false);
      setSent(true); // always show the same result (no account enumeration)
    }
  }

  if (sent) {
    return (
      <AuthCard title="Check your email" subtitle={`If an account exists for ${email}, a reset link is on its way.`}>
        <FormNote>
          <Link className="text-green-deep" to="/login">Back to sign in</Link>
        </FormNote>
      </AuthCard>
    );
  }

  return (
    <AuthCard title="Reset your password" subtitle="Enter your email and we'll send a reset link.">
      <form onSubmit={onSubmit}>
        <TextField label="Email" type="email" value={email} required onChange={(e) => setEmail(e.target.value)} />
        <Button type="submit" variant="primary" className="w-full" disabled={busy}>
          {busy ? "Sending…" : "Send reset link"}
        </Button>
      </form>
      <FormNote>
        <Link className="text-green-deep" to="/login">Back to sign in</Link>
      </FormNote>
    </AuthCard>
  );
}

export function ResetPassword({ api }: { api: Api }) {
  const nav = useNavigate();
  const [params] = useSearchParams();
  const token = params.get("token") ?? "";
  const [password, setPassword] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setErr(null);
    setBusy(true);
    try {
      await api.resetPassword(token, password);
      setDone(true);
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(false);
    }
  }

  if (!token) {
    return <AuthCard title="Invalid reset link" subtitle="This link is missing its token. Request a new one." />;
  }
  if (done) {
    return (
      <AuthCard title="Password updated" subtitle="Your password has been reset.">
        <Button variant="primary" className="w-full mt-2" onClick={() => nav("/login")}>
          Sign in
        </Button>
      </AuthCard>
    );
  }

  return (
    <AuthCard title="Choose a new password">
      <form onSubmit={onSubmit}>
        <FormError message={err} />
        <TextField
          label="New password"
          type="password"
          value={password}
          required
          minLength={8}
          onChange={(e) => setPassword(e.target.value)}
        />
        <Button type="submit" variant="primary" className="w-full" disabled={busy}>
          {busy ? "Updating…" : "Update password"}
        </Button>
      </form>
    </AuthCard>
  );
}
