// The screens of the session itself (ADR-0058), shown outside the router.
import { useEffect, useRef } from "react";

/** While the session starts: no screen renders before it is ready. */
export function SigningIn() {
  return <div className="splash" role="status">Signing in…</div>;
}

export function SignedOut({ onSignIn }: { onSignIn: () => void }) {
  return (
    <section className="full-state">
      <h1>Signed out</h1>
      <p>You have signed out of Spillway.</p>
      <button type="button" onClick={onSignIn}>Sign in</button>
    </section>
  );
}

export function SignInFailed({ reason, onSignIn }: { reason: string; onSignIn: () => void }) {
  return (
    <section className="full-state" role="alert">
      <h1>Sign-in could not be completed</h1>
      <p>{reason[0]?.toUpperCase()}{reason.slice(1)}.</p>
      <button type="button" onClick={onSignIn}>Sign in again</button>
    </section>
  );
}

export function SignInUnavailable() {
  return (
    <section className="full-state" role="alert">
      <h1>Can't reach the sign-in service</h1>
      <p>The control plane or Keycloak did not answer. Check that they are running, then reload.</p>
      <button type="button" onClick={() => location.reload()}>Reload</button>
    </section>
  );
}

export function NoRole({ onSignOut }: { onSignOut: () => void }) {
  return (
    <section className="full-state" role="alert">
      <h1>No Spillway role</h1>
      <p>Your account has no Spillway role; ask an admin.</p>
      <button type="button" onClick={onSignOut}>Sign out</button>
    </section>
  );
}

/** Over the app when the session ends; unsaved edits are lost. */
export function SessionEnded({ onSignIn }: { onSignIn: () => void }) {
  const button = useRef<HTMLButtonElement>(null);
  useEffect(() => button.current?.focus(), []);
  return (
    <div className="modal-backdrop">
      <div className="modal" role="alertdialog" aria-modal="true" aria-labelledby="session-ended" aria-describedby="session-ended-text">
        <h2 id="session-ended">Your session ended</h2>
        <p id="session-ended-text">Sign in again to continue. Changes you have not saved are lost.</p>
        <button type="button" ref={button} onClick={onSignIn}>Sign in again</button>
      </div>
    </div>
  );
}
