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
  const sheetOK = !!me?.sheet_connected;

  return (
    <Routes>
      <Route path="/login" element={authed ? <Navigate to="/" replace /> : <LogIn api={api} reload={reload} />} />
      <Route path="/signup" element={authed ? <Navigate to="/" replace /> : <SignUp api={api} />} />
      <Route path="/verify" element={<VerifyEmailNotice />} />
      <Route path="/forgot" element={<ForgotPassword api={api} />} />
      <Route path="/reset" element={<ResetPassword api={api} />} />
      <Route
        path="/onboarding"
        element={
          !authed ? (
            <Navigate to="/login" replace />
          ) : sheetOK ? (
            <Navigate to="/" replace />
          ) : (
            <Onboarding api={api} me={me!} reload={reload} />
          )
        }
      />
      <Route
        path="*"
        element={
          !authed ? (
            <Navigate to="/login" replace />
          ) : !sheetOK ? (
            <Navigate to="/onboarding" replace />
          ) : (
            <App me={me!} reload={reload} />
          )
        }
      />
    </Routes>
  );
}
