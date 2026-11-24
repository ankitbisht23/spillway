// The policy editor's form (UI-01, CP-03): its values as typed, the body
// they make, the checks the browser can make before the server's (the
// rules of internal/config's compilePolicy that need nothing but the
// policy), and the server's 422 details mapped to the form's fields.
import type { ErrorDetail } from "../../api/errors";
import type { components } from "../../api/schema.gen";

export type Policy = components["schemas"]["Policy"];

export const LEVELS = ["global", "tenant", "consumer", "route"] as const;
export const ALGORITHMS = ["gcra", "sliding_window", "fixed_window", "concurrency"] as const;
export const MODES = ["enforce", "shadow", "off"] as const;
export const FAILURE_MODES = ["fail_open", "fail_closed", "degraded"] as const;

export interface LimitRow {
  rate: string;
  period: string;
  burst: string;
}

export interface PolicyForm {
  name: string;
  level: string;
  /** Route names, comma-separated; "*" for every route. */
  routes: string;
  /** HTTP methods, comma-separated; empty for every method. */
  methods: string;
  keyTemplate: string;
  algorithm: string;
  limits: LimitRow[];
  mode: string;
  local: boolean;
  /** Empty: the server's default, fail_open, for a Redis policy. */
  failureMode: string;
  fromPlan: boolean;
  /** Lease size k (RL-18): empty or 0 for strict. Redis gcra and sliding_window policies only. */
  leaseK: string;
}

export type Field = keyof PolicyForm;
export type FieldErrors = Partial<Record<Field, string>>;

/** A new policy's form: in shadow mode, so it is reviewed before it rejects anything (UI-01). */
export function newForm(): PolicyForm {
  return {
    name: "", level: "route", routes: "", methods: "", keyTemplate: "", algorithm: "gcra",
    limits: [{ rate: "", period: "1s", burst: "" }], mode: "shadow", local: false, failureMode: "", fromPlan: false, leaseK: "",
  };
}

/** The form of a policy as the API answered it. */
export function formOf(p: Policy): PolicyForm {
  return {
    name: p.name,
    level: p.level ?? "",
    routes: (p.match?.routes ?? []).join(", "),
    methods: (p.match?.methods ?? []).join(", "),
    keyTemplate: p.key_template ?? "",
    algorithm: p.algorithm ?? "",
    limits: (p.limits ?? []).map((l) => ({ rate: l.rate === undefined ? "" : String(l.rate), period: l.period ?? "", burst: l.burst ? String(l.burst) : "" })),
    mode: p.mode || "enforce",
    local: p.local ?? false,
    failureMode: p.failure_mode ?? "",
    fromPlan: p.from_plan ?? false,
    leaseK: p.lease_k ? String(p.lease_k) : "",
  };
}

/** Whether a policy can be batched (RL-18): a Redis gcra or sliding_window policy. */
export function leaseable(f: PolicyForm): boolean {
  return !f.local && (f.algorithm === "gcra" || f.algorithm === "sliding_window");
}

function list(s: string): string[] {
  return s.split(",").map((x) => x.trim()).filter(Boolean);
}

/** The body a create or replace sends: the form's fields, none of the read-only ones. */
export function bodyOf(f: PolicyForm): Policy {
  const methods = list(f.methods);
  const body: Record<string, unknown> = {
    name: f.name.trim(),
    level: f.level,
    match: methods.length ? { routes: list(f.routes), methods } : { routes: list(f.routes) },
    key_template: f.keyTemplate.trim(),
    algorithm: f.algorithm,
    mode: f.mode,
  };
  if (f.fromPlan) body.from_plan = true;
  else {
    body.limits = f.limits.map((l) => {
      const out: Record<string, unknown> = { rate: Number(l.rate) };
      if (l.period.trim()) out.period = l.period.trim();
      if (l.burst.trim()) out.burst = Number(l.burst);
      return out;
    });
  }
  if (f.local) body.local = true;
  if (f.failureMode) body.failure_mode = f.failureMode;
  if (leaseable(f) && Number(f.leaseK) > 0) body.lease_k = Number(f.leaseK);
  // The API types read-only fields as required; the server ignores them.
  return body as unknown as Policy;
}

const NAME = /^[A-Za-z0-9._-]{1,64}$/;
const TOKEN = /^[A-Za-z0-9!#$%&'*+\-.^_`|~]+$/;
const KEY_FIELDS = ["ip", "api_key", "consumer", "tenant", "route", "method"];
const MAX_QUANTITY = 1_000_000_000;
const MAX_PERIOD_S = 8760 * 3600;
const MAX_DEFAULT_BURST_PERIOD_S = 60;
const MAX_REDIS_LIMITS = 32;
const MAX_LEASE_K = 1_000_000;

/** The placeholders of a key template, or why it does not parse (as config.ParseKeyTemplate). */
export function parseKeyTemplate(t: string): { fields: string[] } | { error: string } {
  if (!t) return { error: "Required, such as {ip} or {consumer}" };
  const fields: string[] = [];
  for (let s = t; s !== ""; ) {
    const i = s.search(/[{}]/);
    if (i < 0) break;
    if (s[i] === "}") return { error: "'}' without '{'" };
    const j = s.indexOf("}", i);
    if (j < 0) return { error: "'{' without '}'" };
    const name = s.slice(i + 1, j);
    s = s.slice(j + 1);
    if (name.startsWith("header:")) {
      if (!TOKEN.test(name.slice("header:".length))) return { error: `{${name}}: not a valid header name` };
    } else if (!KEY_FIELDS.includes(name)) {
      return { error: `Unknown placeholder {${name}}; want {ip}, {api_key}, {consumer}, {tenant}, {route}, {method} or {header:<Name>}` };
    }
    fields.push(name);
  }
  return { fields };
}

const UNITS: Record<string, number> = { ns: 1e-9, us: 1e-6, "µs": 1e-6, "μs": 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };

/** A Go duration (time.ParseDuration) in seconds, or null. */
export function parseDuration(s: string): number | null {
  if (s === "0") return 0;
  const re = /(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/gy;
  let total = 0;
  let at = 0;
  for (let m = re.exec(s); m; m = re.exec(s)) {
    total += Number(m[1]) * (UNITS[m[2] ?? ""] ?? NaN);
    at = re.lastIndex;
  }
  return s !== "" && at === s.length && Number.isFinite(total) ? total : null;
}

function wholeNumber(s: string, min: number): boolean {
  return /^\d+$/.test(s.trim()) && Number(s) >= min && Number(s) <= MAX_QUANTITY;
}

/** Why the limits are not valid, if they are not (as config's compileLimits). */
function limitsError(f: PolicyForm, local: boolean): string | undefined {
  const rows = f.limits;
  if (rows.length === 0) return "At least one limit is required";
  if (f.algorithm === "concurrency" && (rows.length !== 1 || rows[0]?.period.trim() || rows[0]?.burst.trim())) {
    return "Concurrency takes exactly one limit, with only a rate (the most in flight)";
  }
  if (!local && rows.length > MAX_REDIS_LIMITS) return `A Redis policy has at most ${MAX_REDIS_LIMITS} limits`;
  const periods = new Set<number>();
  for (const [i, l] of rows.entries()) {
    const n = `Limit ${i + 1}`;
    if (!wholeNumber(l.rate, 1)) return `${n}: rate must be a whole number from 1 to ${MAX_QUANTITY}`;
    if (f.algorithm === "concurrency") continue;
    const p = parseDuration(l.period.trim());
    if (p === null || p < 1 || p > MAX_PERIOD_S || !Number.isInteger(p)) return `${n}: period must be whole seconds from 1s to 8760h, such as 1s, 1m or 1h`;
    if (periods.has(p)) return `${n}: another limit has the same period`;
    periods.add(p);
    if (f.algorithm !== "gcra") {
      if (l.burst.trim()) return `${n}: only gcra has a burst`;
      continue;
    }
    if (!l.burst.trim() && p > MAX_DEFAULT_BURST_PERIOD_S) return `${n}: a burst is required for periods over 1m`;
    if (l.burst.trim() && !wholeNumber(l.burst, 1)) return `${n}: burst must be a whole number from 1 to ${MAX_QUANTITY}`;
  }
  return undefined;
}

/**
 * What the browser can check before a save: each field's first problem.
 * The server checks again, with what the browser cannot know (that the
 * routes exist, route costs, conflicts at publish).
 */
export function validatePolicy(f: PolicyForm): FieldErrors {
  const e: FieldErrors = {};
  if (!NAME.test(f.name.trim())) e.name = "1–64 characters from A–Z, a–z, 0–9, '.', '_' and '-'";
  if (!(LEVELS as readonly string[]).includes(f.level)) e.level = "Choose a level";
  if (!(ALGORITHMS as readonly string[]).includes(f.algorithm)) e.algorithm = "Choose an algorithm";
  if (!(MODES as readonly string[]).includes(f.mode)) e.mode = "Choose a mode";
  const routes = list(f.routes);
  if (routes.length === 0) e.routes = 'Required: route names, or * for every route';
  else if (routes.includes("*") && routes.length > 1) e.routes = '"*" must be the only entry';
  else {
    const twice = routes.find((r, i) => routes.indexOf(r) !== i);
    if (twice) e.routes = `"${twice}" is listed twice`;
  }
  const tmpl = parseKeyTemplate(f.keyTemplate.trim());
  if ("error" in tmpl) e.keyTemplate = tmpl.error;
  const local = f.local || f.algorithm === "concurrency";
  if (local && f.failureMode) e.failureMode = "Only Redis policies have a failure mode; local policies never fail";
  if (!local && (f.algorithm === "fixed_window" || f.algorithm === "concurrency") && !e.algorithm) {
    e.algorithm = `${f.algorithm} must be local; Redis policies use gcra or sliding_window`;
  }
  if (f.fromPlan) {
    const perConsumer = (f.level === "consumer" || f.level === "route") && "fields" in tmpl && tmpl.fields.some((x) => x === "consumer" || x === "api_key");
    if (!perConsumer) e.fromPlan = "Plans apply only to consumer- and route-level policies whose key template has {consumer} or {api_key}";
  } else {
    const err = limitsError(f, local);
    if (err) e.limits = err;
  }
  const lease = leaseKError(f);
  if (lease) e.leaseK = lease;
  return e;
}

/** Why the lease size is not valid, if it is not (as config's compilePolicy and checkLeaseK; plans are the server's to check). */
function leaseKError(f: PolicyForm): string | undefined {
  const k = f.leaseK.trim();
  if (!leaseable(f) || k === "") return undefined;
  if (!/^\d+$/.test(k) || Number(k) > MAX_LEASE_K) return `A whole number from 0 to ${MAX_LEASE_K}`;
  if (Number(k) === 0 || f.fromPlan) return undefined;
  for (const [i, l] of f.limits.entries()) {
    const at = f.algorithm === "gcra" ? Number(l.burst.trim() || l.rate) : Number(l.rate);
    if (Number.isFinite(at) && at >= 1 && Number(k) > at) {
      return `At most limit ${i + 1}'s ${f.algorithm === "gcra" ? "burst" : "rate"} of ${at}: a lease holds at most what a limit admits at once`;
    }
  }
  return undefined;
}

/** The form field a server 422 detail is about, from its path or its message's first word. */
const SERVER_FIELDS: [RegExp, Field][] = [
  [/^name\b/, "name"], [/^level\b/, "level"], [/^algorithm\b/, "algorithm"], [/^mode\b/, "mode"],
  [/^failure_mode\b/, "failureMode"], [/^match\.routes\b/, "routes"], [/^match\.methods\b/, "methods"],
  [/^key_template\b/, "keyTemplate"], [/^limits\b/, "limits"], [/^route "/, "limits"], [/^from_plan\b/, "fromPlan"],
  [/^lease_k\b/, "leaseK"],
];

/**
 * The server's validation details placed on the form's fields (UI-01,
 * ADR-0060): by path when it has one, else by the message's leading field
 * name. Details that name no field stay in the rest.
 */
export function serverErrors(details: readonly ErrorDetail[]): { fields: FieldErrors; rest: string[] } {
  const fields: FieldErrors = {};
  const rest: string[] = [];
  for (const d of details) {
    const msg = d.message ?? "";
    const field = SERVER_FIELDS.find(([re]) => re.test(d.path ?? "") || re.test(msg))?.[1];
    if (field && !fields[field]) fields[field] = msg;
    else rest.push(msg);
  }
  return { fields, rest };
}
