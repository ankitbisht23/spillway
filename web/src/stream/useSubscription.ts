// A screen's live data (UI-03, ADR-0059): one stream with the screen's
// subscriptions, the latest result and error per subscription in component
// state (not the query cache), closed on unmount. A change of
// subscriptions or parameters opens a new stream and starts empty.
import { useEffect, useState } from "react";

import {
  browserStreamDeps, openStream, streamQuery,
  type StreamDeps, type StreamError, type StreamParams, type StreamQueryName, type StreamResult, type StreamStatus,
} from "./client";

export interface Subscription {
  /** The latest result, kept while an error or a reconnect follows it. */
  result?: StreamResult;
  /** The latest evaluation's error; cleared by the next result. */
  error?: StreamError;
}

export interface SubscriptionState {
  status: StreamStatus;
  subs: Partial<Record<StreamQueryName, Subscription>>;
}

interface Keyed extends SubscriptionState {
  query: string;
}

const initial: SubscriptionState = { status: { state: "connecting" }, subs: {} };

export function useSubscription(
  subs: readonly StreamQueryName[],
  params: StreamParams = {},
  deps: () => StreamDeps = browserStreamDeps,
): SubscriptionState {
  const query = streamQuery(subs, params);
  const [state, setState] = useState<Keyed>({ query, ...initial });

  useEffect(() => {
    // Updates for another query (a stream being replaced) are dropped.
    const update = (fn: (s: SubscriptionState) => SubscriptionState) =>
      setState((prev) => (prev.query === query ? { query, ...fn(prev) } : { query, ...fn(initial) }));
    return openStream(query, {
      onResult: (r) => update((s) => ({ ...s, subs: { ...s.subs, [r.sub]: { result: r } } })),
      onError: (e) => update((s) => ({ ...s, subs: { ...s.subs, [e.sub]: { result: s.subs[e.sub]?.result, error: e } } })),
      onStatus: (status) => update((s) => ({ ...s, status })),
    }, deps());
  }, [query, deps]);

  return state.query === query ? { status: state.status, subs: state.subs } : initial;
}
