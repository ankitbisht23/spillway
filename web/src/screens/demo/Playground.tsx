// The Playground (UI-07, SC-04, ADR-0096 §3), shown only in demo mode
// (SC-08, ADR-0095 §2): its URL is Not found unless GET /api/v1/demo
// answers. Every role may fire traffic or run a scenario (D1–D3); the
// control plane sends it to the demo gateways with its own short-lived
// keys, and this screen only reads the results over the run's SSE stream,
// same origin. Admins also get the D5 and D6 fault switches (SC-03). The
// screen never sees a key and never writes to the console.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useReducer, useRef, useState, type FormEvent } from "react";

import { api, call } from "../../api/client";
import { ApiError } from "../../api/errors";
import type { components } from "../../api/schema.gen";
import { useCan } from "../../auth/SessionContext";
import { LineChart } from "../../charts/LineChart";
import type { Aligned } from "../../charts/series";
import { Dialog } from "../../components/Dialog";
import { ErrorLine } from "../../components/ErrorLine";
import { Empty, Loading, NotFound } from "../../components/states";
import { limitOf, perSecond, remaining } from "./ratelimit";
import { openRunStream, type PlaygroundEnd, type PlaygroundRun, type PlaygroundTick } from "./runStream";
import { useDemo } from "./useDemo";

type Demo = components["schemas"]["Demo"];
type DemoFault = components["schemas"]["DemoFault"];
type Counts = components["schemas"]["PlaygroundCounts"];
type Last = components["schemas"]["PlaygroundLast"];
type Tenant = NonNullable<components["schemas"]["PlaygroundRunRequest"]["tenant"]>;
type Route = NonNullable<components["schemas"]["PlaygroundRunRequest"]["route"]>;
type Scenario = NonNullable<components["schemas"]["PlaygroundRunRequest"]["scenario"]>;

const TENANTS: readonly Tenant[] = ["free-co", "pro-co", "enterprise-co"];
const ROUTES: readonly Route[] = ["orders", "catalog", "payments"];
/** The traffic scenarios, with their stories from the design's table and the route they use. */
const SCENARIOS: readonly { id: Scenario; title: string; story: string; route: Route }[] = [
  { id: "d1", title: "D1 Fair limits by plan", story: "Free tenant gets 429 with RateLimit headers at its limit; pro keeps flowing.", route: "orders" },
  { id: "d2", title: "D2 Noisy neighbour", story: "One tenant floods at 50× its limit; only it is throttled, others' p99 stays flat.", route: "orders" },
  { id: "d3", title: "D3 Distributed accuracy", story: "3 replicas behind a round-robin load balancer; allowed rate tracks the limit line.", route: "payments" },
];
const FAULTS: Record<DemoFault["name"], string> = { "redis-cut": "Redis cut (D5)", "upstream-slow": "Upstream slow (D6)" };
/** Ticks kept per tenant: a run is at most 60 s. */
const MAX_TICKS = 70;

/** getDemo again every 5 s while the screen is open, for the fault states and the caller's runs left. */
function useDemoLive() {
  return useQuery({ queryKey: ["demo", "live"], queryFn: async () => (await call(api.GET("/demo"))).data, refetchInterval: 5_000 });
}

interface Series {
  times: number[];
  ok: number[];
  limited: number[];
}

interface RunState {
  phase: "idle" | "starting" | "running" | "ended" | "error";
  run?: PlaygroundRun;
  error?: unknown;
  series: Record<string, Series>;
  last: Record<string, Last>;
  replicas: Record<string, number>;
  totals?: Record<string, Counts>;
  reason?: PlaygroundEnd["reason"];
}

type Action =
  | { type: "starting" }
  | { type: "started"; run: PlaygroundRun }
  | { type: "tick"; tick: PlaygroundTick }
  | { type: "end"; end: PlaygroundEnd }
  | { type: "error"; error: unknown };

const initial: RunState = { phase: "idle", series: {}, last: {}, replicas: {} };

function reduce(s: RunState, a: Action): RunState {
  switch (a.type) {
    case "starting":
      return { ...initial, phase: "starting" };
    case "started":
      return { ...initial, phase: "running", run: a.run };
    case "tick": {
      const t = Date.parse(a.tick.t) / 1000;
      const series = { ...s.series };
      for (const [tenant, c] of Object.entries(a.tick.tenants)) {
        const prev = series[tenant] ?? { times: [], ok: [], limited: [] };
        series[tenant] = { times: [...prev.times, t].slice(-MAX_TICKS), ok: [...prev.ok, c.ok].slice(-MAX_TICKS), limited: [...prev.limited, c.limited].slice(-MAX_TICKS) };
      }
      const replicas = { ...s.replicas };
      for (const [r, n] of Object.entries(a.tick.replicas)) replicas[r] = (replicas[r] ?? 0) + n;
      return { ...s, series, replicas, last: { ...s.last, ...a.tick.last } };
    }
    case "end":
      return { ...s, phase: "ended", totals: a.end.totals, reason: a.end.reason };
    case "error":
      return { ...s, phase: "error", error: a.error };
  }
}

/** The tenants and routes a run sends: its own for a custom run, the scenario's otherwise. */
function routeOf(run: PlaygroundRun | undefined): Route {
  if (!run) return "orders";
  if (run.route) return run.route as Route;
  return SCENARIOS.find((s) => s.id === run.scenario)?.route ?? "orders";
}

function runTitle(run: PlaygroundRun): string {
  if (run.kind === "scenario") return SCENARIOS.find((s) => s.id === run.scenario)?.title ?? `Scenario ${run.scenario ?? ""}`;
  return `${run.tenant ?? ""} on ${run.route ?? ""} at ${run.rate ?? 0} requests/s for ${run.duration_s} s`;
}

/** A start's or stop's error, with a cap's Retry-After. */
function RunError({ error }: { error: unknown }) {
  if (error instanceof ApiError && error.status === 429) {
    return (
      <p className="form-error" role="alert">
        Too many runs: {error.message}
        {error.retryAfter !== undefined && <> Try again in {error.retryAfter} s.</>}
      </p>
    );
  }
  return <ErrorLine error={error} />;
}

export function Playground() {
  const demo = useDemo();
  if (demo.status === "pending") return <Loading />;
  if (demo.status === "error") return <NotFound />;
  return <PlaygroundScreen demo={demo.data} />;
}

function PlaygroundScreen({ demo: probed }: { demo: Demo }) {
  const live = useDemoLive();
  const demo = live.data ?? probed;
  const queryClient = useQueryClient();
  const [state, dispatch] = useReducer(reduce, initial);
  const closeStream = useRef<(() => void) | null>(null);
  const [tenant, setTenant] = useState<Tenant>("free-co");
  const [route, setRoute] = useState<Route>("orders");
  const [rate, setRate] = useState(30);
  const [duration, setDuration] = useState(30);

  useEffect(() => () => closeStream.current?.(), []);

  const watch = (run: PlaygroundRun) => {
    closeStream.current?.();
    dispatch({ type: "started", run });
    closeStream.current = openRunStream(run.id, {
      onReady: () => {},
      onTick: (tick) => dispatch({ type: "tick", tick }),
      onEnd: (end) => {
        dispatch({ type: "end", end });
        void queryClient.invalidateQueries({ queryKey: ["demo", "live"] });
      },
      onError: (error) => dispatch({ type: "error", error }),
    });
  };

  const start = useMutation({
    mutationFn: async ({ body, previous }: { body: components["schemas"]["PlaygroundRunRequest"]; previous: string | undefined }) => {
      // One run at a time per address (ADR-0096 §3): a new run stops the
      // caller's running one first.
      if (previous) await api.DELETE("/demo/playground/runs/{id}", { params: { path: { id: previous } } }).catch(() => undefined);
      return (await call(api.POST("/demo/playground/runs", { body }))).data;
    },
    onMutate: () => {
      closeStream.current?.();
      closeStream.current = null;
      dispatch({ type: "starting" });
    },
    onSuccess: (run) => {
      watch(run);
      void queryClient.invalidateQueries({ queryKey: ["demo", "live"] });
    },
    onError: (error) => dispatch({ type: "error", error }),
  });
  const stop = useMutation({
    mutationFn: async (id: string) => (await call(api.DELETE("/demo/playground/runs/{id}", { params: { path: { id } } }))).data,
  });

  const begin = (body: components["schemas"]["PlaygroundRunRequest"]) =>
    start.mutate({ body, previous: state.phase === "running" ? state.run?.id : undefined });
  const fire = (e: FormEvent) => {
    e.preventDefault();
    begin({ kind: "custom", tenant, route, rate, duration_s: duration });
  };
  const running = state.phase === "running" || state.phase === "starting";
  const pg = demo.playground;

  return (
    <section>
      <h1>Playground</h1>
      <p className="muted">
        Traffic is sent by the control plane to the demo gateways with its own short-lived keys; this page only shows the results. You can start {pg.runs_left_this_hour} more
        {pg.runs_left_this_hour === 1 ? " run" : " runs"} this hour.
      </p>
      <div className="cards">
        <form className="card" onSubmit={fire} aria-label="Fire traffic">
          <h2>Fire traffic</h2>
          <label className="field">
            Tenant
            <select value={tenant} onChange={(e) => setTenant(e.target.value as Tenant)}>
              {TENANTS.map((t) => {
                const info = demo.tenants.find((x) => x.name === t);
                return <option key={t} value={t}>{info?.plan ? `${t} (${info.plan}${info.limit ? `, ${info.limit.rate} per ${info.limit.period}` : ""})` : t}</option>;
              })}
            </select>
          </label>
          <label className="field">
            Route
            <select value={route} onChange={(e) => setRoute(e.target.value as Route)}>
              {ROUTES.map((r) => <option key={r} value={r}>{r}</option>)}
            </select>
          </label>
          <label className="field">
            Rate: {rate} requests/s
            <input type="range" min={1} max={pg.max_rate} value={rate} onChange={(e) => setRate(Number(e.target.value))} />
          </label>
          <label className="field">
            Duration (seconds)
            <input type="number" min={5} max={pg.max_duration_s} value={duration} onChange={(e) => setDuration(Number(e.target.value))} />
          </label>
          <div className="actions">
            <button type="submit" disabled={start.isPending}>Fire</button>
            <button type="button" className="secondary" disabled={!state.run || state.phase !== "running" || stop.isPending} onClick={() => state.run && stop.mutate(state.run.id)}>
              Stop
            </button>
          </div>
          {stop.isError && <RunError error={stop.error} />}
        </form>
        <div className="card">
          <h2>Run a scenario</h2>
          <ul className="scenarios">
            {SCENARIOS.map((s) => (
              <li key={s.id}>
                <button type="button" disabled={start.isPending} onClick={() => begin({ kind: "scenario", scenario: s.id })}>{s.title}</button>
                <span className="muted"> {s.story}</span>
              </li>
            ))}
          </ul>
        </div>
        <FaultSwitches faults={demo.faults} />
      </div>
      <Results state={state} demo={demo} running={running} />
    </section>
  );
}

function Results({ state, demo, running }: { state: RunState; demo: Demo; running: boolean }) {
  if (state.phase === "idle") return <Empty>Fire some traffic, or run a scenario, to see allowed and denied requests per tenant.</Empty>;
  if (state.phase === "starting") return <Loading label="Starting the run" />;
  if (state.phase === "error" && !state.run) return <RunError error={state.error} />;
  const run = state.run;
  const route = routeOf(run);
  const tenants = TENANTS.filter((t) => state.series[t] || state.last[t] || state.totals?.[t]);
  return (
    <section aria-label="Results">
      <h2>{run ? runTitle(run) : "Run"}</h2>
      <p className="stream-status" role="status">
        {state.phase === "running" && "Running: live over the run's stream."}
        {state.phase === "ended" && `Ended (${state.reason ?? "done"}).`}
        {state.phase === "error" && "The stream stopped."}
      </p>
      {state.phase === "error" && <RunError error={state.error} />}
      {tenants.length === 0 && running && <Empty>Waiting for the first second of results.</Empty>}
      <p className="muted">
        Requests per replica:{" "}
        {Object.entries(state.replicas).sort(([a], [b]) => a.localeCompare(b)).map(([r, n]) => `${r} ${n}`).join(", ") || "none yet"}
      </p>
      <div className="cards">
        {tenants.map((t) => <TenantCard key={t} tenant={t} route={route} demo={demo} state={state} />)}
      </div>
    </section>
  );
}

function TenantCard({ tenant, route, demo, state }: { tenant: string; route: Route; demo: Demo; state: RunState }) {
  const s = state.series[tenant] ?? { times: [], ok: [], limited: [] };
  const { policy, limit } = limitOf(demo, tenant, route);
  const line = perSecond(limit);
  const data: Aligned = {
    times: s.times,
    lines: [
      { label: "allowed", values: s.ok },
      { label: "denied (429)", values: s.limited },
      ...(line === null ? [] : [{ label: "limit", values: s.times.map(() => line) }]),
    ],
  };
  const last = state.last[tenant];
  const tokens = policy ? remaining(last?.ratelimit, policy) : undefined;
  const total = state.totals?.[tenant];
  return (
    <article className="card" aria-label={tenant}>
      <h3>{tenant}</h3>
      <LineChart title={`${tenant}: allowed and denied per second`} data={data} unit="rps" tone={(l) => (l === "allowed" ? 1 : l === "limit" ? 6 : 3)} />
      <h4>RateLimit headers</h4>
      {last ? (
        <dl className="facts headers">
          <dt>Status</dt><dd>{last.status} from {last.replica}</dd>
          <dt>RateLimit</dt><dd><code>{last.ratelimit ?? "–"}</code></dd>
          <dt>RateLimit-Policy</dt><dd><code>{last.ratelimit_policy ?? "–"}</code></dd>
          <dt>Retry-After</dt><dd><code>{last.retry_after ?? "–"}</code></dd>
        </dl>
      ) : <p className="muted">No answer yet.</p>}
      <h4>Token bucket</h4>
      {limit && policy && tokens !== undefined ? (
        <p className="bucket">
          <meter min={0} max={limit.burst} value={Math.min(tokens, limit.burst)} aria-label={`${tenant} token bucket`} />{" "}
          {tokens} of {limit.burst} tokens left in <code>{policy}</code>
        </p>
      ) : <p className="muted">No {policy ?? "limit"} item in the latest answer.</p>}
      {total && (
        <p className="muted totals">
          Whole run: {total.sent} sent, {total.ok} allowed, {total.limited} denied, {total.forbidden} forbidden, {total.failed} failed, {total.other} other
          {total.dropped > 0 && `, ${total.dropped} dropped`}; p99 {total.p99_ms === null ? "–" : `${total.p99_ms.toFixed(1)} ms`}
        </p>
      )}
    </article>
  );
}

/** The D5 and D6 fault switches, for admins only (SC-03, P42); the server checks the role again. */
function FaultSwitches({ faults }: { faults: readonly DemoFault[] }) {
  const can = useCan("setDemoFault");
  const [open, setOpen] = useState<DemoFault | null>(null);
  if (!can.allowed) return null;
  return (
    <div className="card">
      <h2>Faults</h2>
      <ul className="faults">
        {faults.map((f) => (
          <li key={f.name}>
            {FAULTS[f.name]}: <strong>{f.active === null ? "unknown" : f.active ? "on" : "off"}</strong>{" "}
            <button type="button" className={f.active ? "secondary" : "danger"} onClick={() => setOpen(f)}>
              {f.active ? "Switch off" : "Switch on"}
            </button>
          </li>
        ))}
      </ul>
      {open && <FaultDialog fault={open} onClose={() => setOpen(null)} />}
    </div>
  );
}

function FaultDialog({ fault, onClose }: { fault: DemoFault; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [note, setNote] = useState("");
  const active = !fault.active;
  const change = useMutation({
    mutationFn: async () =>
      (await call(api.PUT("/demo/faults/{name}", { params: { path: { name: fault.name } }, body: note.trim() ? { active, note: note.trim() } : { active } }))).data,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["demo", "live"] });
      onClose();
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    change.mutate();
  };
  return (
    <Dialog title={`Switch ${FAULTS[fault.name]} ${active ? "on" : "off"}`} onClose={onClose}>
      <form onSubmit={submit}>
        <p>
          {fault.name === "redis-cut"
            ? "Cuts the demo gateways' Redis traffic at toxiproxy: each policy takes its failure mode."
            : "Delays the orders upstream by 4 s at toxiproxy: the breaker and the retry budget take over."}{" "}
          It affects every visitor of the demo until it is switched off, and goes into the audit log.
        </p>
        <label className="field">
          Note (optional)
          <input value={note} maxLength={500} onChange={(e) => setNote(e.target.value)} />
        </label>
        {change.isError && <ErrorLine error={change.error} needs={["admin"]} />}
        <div className="actions">
          <button type="submit" className={active ? "danger" : undefined} disabled={change.isPending}>{active ? "Switch on" : "Switch off"}</button>
          <button type="button" className="secondary" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Dialog>
  );
}
