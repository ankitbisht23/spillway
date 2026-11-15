// A live screen's states (UI-03, UI-06, ADR-0059, ADR-0060): the stream's
// status line, and per subscription loading, error, empty or its content;
// while the stream reconnects or a subscription's latest evaluation failed,
// the last data stays shown, greyed, with the reason.
import type { ReactNode } from "react";

import type { StreamError, StreamStatus } from "../stream/client";
import type { Subscription } from "../stream/useSubscription";
import { Empty, Loading } from "./states";

/** What a failed evaluation means for the reader. */
export function streamErrorMessage(e: StreamError): string {
  switch (e.code) {
    case "prometheus_unavailable":
      return "Prometheus is unavailable.";
    case "prometheus_not_configured":
      return "Prometheus is not configured on the control plane.";
    case "query_failed":
      return `The query failed: ${e.message}`;
    case "unavailable":
      return "The control plane's database is unavailable.";
  }
}

function time(at: string): string {
  const d = new Date(at);
  return Number.isNaN(d.getTime()) ? at : d.toLocaleTimeString("en-GB");
}

/** The stream's status, beside the filters: live, connecting, reconnecting or paused. */
export function StreamStatusLine({ status, at }: { status: StreamStatus; at: string | undefined }) {
  let text: string;
  switch (status.state) {
    case "open":
      text = at ? `Live · updated ${time(at)}` : "Live";
      break;
    case "connecting":
      text = "Connecting…";
      break;
    case "reconnecting":
      text = "Reconnecting…";
      break;
    case "paused":
      text = "Paused while the tab is hidden";
      break;
    case "failed":
      text = "Stopped";
      break;
  }
  return (
    <p className={`stream-status stream-${status.state}`} role="status">
      {text}
    </p>
  );
}

interface Props<T> {
  /** What the subscription shows, for the loading state's name. */
  title: string;
  sub: Subscription | undefined;
  /** The stream is not open: the last data is shown greyed. */
  stale: boolean;
  /** The subscription's data as the content needs it; null when its shape is not that. */
  select: (data: unknown) => T | null;
  isEmpty: (data: T) => boolean;
  empty: ReactNode;
  children: (data: T) => ReactNode;
}

/** One subscription's loading, error, empty or content state. */
export function LiveState<T>({ title, sub, stale, select, isEmpty, empty, children }: Props<T>) {
  if (!sub?.result) {
    if (sub?.error) {
      return (
        <div className="state state-error" role="alert">
          <p>{streamErrorMessage(sub.error)}</p>
          <p className="muted">Retried every 5 s.</p>
        </div>
      );
    }
    return <Loading label={`Loading ${title}`} />;
  }
  const data = select(sub.result.data);
  const body = data === null || isEmpty(data) ? <Empty>{empty}</Empty> : children(data);
  const greyed = stale || sub.error !== undefined;
  return (
    <div className={greyed ? "live live-stale" : "live"}>
      {sub.error && (
        <p className="live-error" role="alert">
          {streamErrorMessage(sub.error)} Showing data from {time(sub.result.at)}.
        </p>
      )}
      {body}
    </div>
  );
}

/** The newest result time of the subscriptions, for the status line. */
export function newestAt(subs: Partial<Record<string, Subscription>>): string | undefined {
  let newest: string | undefined;
  for (const s of Object.values(subs)) {
    const at = s?.result?.at;
    if (at && (!newest || at > newest)) newest = at;
  }
  return newest;
}
