// The top of the app (UI-08, ADR-0058): what boot found decides the
// screen; with a session, /me's roles are known before any screen renders.
import { QueryClientProvider, useQuery, type QueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState, useSyncExternalStore } from "react";

import { api, call } from "../api/client";
import { ApiError, describeError } from "../api/errors";
import type { Boot } from "../auth/boot";
import { HOME_PATH, SIGNED_OUT_PATH, type Provider } from "../auth/oidc";
import type { Session } from "../auth/session";
import { SessionContext } from "../auth/SessionContext";
import { ErrorState } from "../components/states";
import { NoRole, SessionEnded, SignedOut, SignInFailed, SignInUnavailable, SigningIn } from "../screens/session";
import { App } from "./App";
import type { createAppRouter } from "./router";

interface Props {
  boot: Boot;
  queryClient: QueryClient;
  createRouter: () => ReturnType<typeof createAppRouter>;
  /** Starts a login that comes back to returnTo (a navigation away). */
  signIn: (provider: Provider, returnTo: string) => void;
}

export function Root({ boot, queryClient, createRouter, signIn }: Props) {
  switch (boot.kind) {
    case "redirecting":
      return <SigningIn />;
    case "unavailable":
      return <SignInUnavailable />;
    case "signed-out":
      return <SignedOut onSignIn={() => signIn(boot.provider, HOME_PATH)} />;
    case "sign-in-failed":
      return <SignInFailed reason={boot.reason} onSignIn={() => signIn(boot.provider, HOME_PATH)} />;
    case "session":
      return (
        <QueryClientProvider client={queryClient}>
          <SessionRoot session={boot.session} provider={boot.provider} queryClient={queryClient} createRouter={createRouter} signIn={signIn} />
        </QueryClientProvider>
      );
  }
}

function SessionRoot({ session, provider, queryClient, createRouter, signIn }: Omit<Props, "boot"> & { session: Session; provider: Provider }) {
  const status = useSyncExternalStore(session.subscribe, session.status);
  const me = useQuery({ queryKey: ["me"], queryFn: async () => (await call(api.GET("/me"))).data, staleTime: Infinity });
  const [router] = useState(createRouter);
  const view = useMemo(() => (me.data ? { user: me.data, signOut: () => session.signOut() } : null), [me.data, session]);

  useEffect(() => {
    if (status !== "signed-out") return;
    queryClient.clear();
    history.replaceState(null, "", SIGNED_OUT_PATH);
  }, [status, queryClient]);

  const signInAgain = () => signIn(provider, location.pathname + location.search);
  if (status === "signed-out") return <SignedOut onSignIn={() => signIn(provider, HOME_PATH)} />;
  if (!view) {
    if (status === "ended") return <SessionEnded onSignIn={signInAgain} />;
    if (me.status === "pending") return <SigningIn />;
    if (me.error instanceof ApiError && me.error.status === 403) return <NoRole onSignOut={() => session.signOut()} />;
    return (
      <section className="full-state">
        <ErrorState view={describeError(me.error)} onRetry={() => void me.refetch()} />
      </section>
    );
  }
  return (
    <SessionContext value={view}>
      <App router={router} queryClient={queryClient} />
      {status === "ended" && <SessionEnded onSignIn={signInAgain} />}
    </SessionContext>
  );
}
