import { useCallback, useEffect, useState } from "react";
import { Navigate, Route, Routes } from "react-router-dom";
import { api } from "./api";
import { App } from "./App";
import {
  ForgotPassword,
  LogIn,
  ResetPassword,
  SignUp,
  VerifyEmailNotice,
} from "./components/auth/AuthScreens";
import { NotionSetup } from "./components/onboarding/NotionSetup";
import { Onboarding } from "./components/onboarding/Onboarding";
import { Me } from "./types";

// Root bootstraps the session (GET /api/me) and routes: unauthenticated users
// to the auth screens, authenticated-but-not-yet-connected users to onboarding,
// and everyone else into the app. Email links (/verify, /reset) are plain
// routes so they work when opened cold.
export function Root() {
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(true);

  const reload = useCallback(async () => {
    try {
      setMe(await api.me());
    } catch {
      setMe(null);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    reload();
  }, [reload]);

  if (loading) {
    return <div className="min-h-dvh flex items-center justify-center text-text-2">Loading…</div>;
  }

  const authed = !!me?.authenticated;
  // Onboarding is done once a destination has been chosen at all. A broken one
  // still counts: that user needs a reconnect prompt inside the app, not the
  // setup flow again, and they can keep submitting and reviewing meanwhile.
  const destinationOK = !!me?.destination_configured;

  return (
    <Routes>
      <Route path="/login" element={authed ? <Navigate to="/" replace /> : <LogIn api={api} reload={reload} />} />
      <Route path="/signup" element={authed ? <Navigate to="/" replace /> : <SignUp api={api} />} />
      <Route path="/verify" element={<VerifyEmailNotice />} />
      <Route path="/forgot" element={<ForgotPassword api={api} />} />
      <Route path="/reset" element={<ResetPassword api={api} />} />
      {/* Reachable with a destination already connected: this is also the
          "change destination" screen, reached deliberately from the account
          menu, so it is not redirected away once onboarding is done. */}
      <Route
        path="/onboarding"
        element={!authed ? <Navigate to="/login" replace /> : <Onboarding api={api} me={me!} reload={reload} />}
      />
      {/* The Notion connect + database + mapping steps. Single-segment like
          every other route: the bundle uses relative asset URLs, so a nested
          path would resolve them against the wrong directory and the app
          would not boot on a deep link or a refresh. */}
      <Route
        path="/onboarding-notion"
        element={
          !authed ? <Navigate to="/login" replace /> : <NotionSetup api={api} me={me!} reload={reload} />
        }
      />
      <Route
        path="*"
        element={
          !authed ? (
            <Navigate to="/login" replace />
          ) : !destinationOK ? (
            <Navigate to="/onboarding" replace />
          ) : (
            <App me={me!} reload={reload} />
          )
        }
      />
    </Routes>
  );
}
