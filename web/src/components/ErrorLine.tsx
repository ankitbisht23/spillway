import { describeError } from "../api/errors";

/** A failed action's error in one line, with the request ID (UI-06). needs: the roles named on a 403. */
export function ErrorLine({ error, needs }: { error: unknown; needs?: readonly string[] }) {
  const view = describeError(error, needs);
  return (
    <p className="form-error" role="alert">
      {view.title}: {view.message}
      {view.requestId && <> (request ID <code>{view.requestId}</code>)</>}
    </p>
  );
}
