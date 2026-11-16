// The route and upstream forms (UI-06, CP-14, ADR-0064): each form is a
// list of fields, each bound to a path of the entity. A form starts from
// the entity as the API answered it; a save sends that entity with each
// field's value at its path, an empty field leaving its setting unset, so
// a setting the form does not show is sent back as it was, never dropped.
// The browser checks only what a field's kind says (a whole number, a
// name); the server's rules stay the server's, and its 422 details land on
// the fields they name.
import type { ErrorDetail } from "../../api/errors";
import type { components } from "../../api/schema.gen";

export type Route = components["schemas"]["Route"];
export type Upstream = components["schemas"]["Upstream"];
export type Endpoint = components["schemas"]["Endpoint"];

/**
 * How a field shows its setting: text and select as typed; int and number
 * parsed; bool a checkbox; list comma-separated; headers one "Name: value"
 * a line; headerMatch one "Name: value" (exact) or "Name ~ regex" a line;
 * endpoints one host:port a line.
 */
export type Kind = "text" | "select" | "int" | "number" | "bool" | "list" | "headers" | "headerMatch" | "endpoints";

export interface FieldSpec {
  /** Where the setting is in the entity, dot-separated. */
  path: string;
  label: string;
  kind: Kind;
  /** The fieldset it is shown in; none for the form's top. */
  group?: string;
  hint?: string;
  required?: boolean;
  /** A select's choices; its empty choice is the default. */
  options?: readonly string[];
}

export type Values = Record<string, string | boolean>;
export type FieldErrors = Record<string, string>;

const NAME_RE = /^[A-Za-z0-9._-]{1,64}$/;
const NAME_RULE = "1–64 characters from A–Z, a–z, 0–9, '.', '_' and '-'";

const DURATION = "A Go duration such as 200ms, 5s or 1m.";

export const AUTH_MODES = ["none", "api_key", "jwt", "either"] as const;
export const LB = ["round_robin", "p2c", "consistent_hash"] as const;

export const ROUTE_FIELDS: readonly FieldSpec[] = [
  { path: "name", label: "Name", kind: "text", required: true },
  { path: "upstream", label: "Upstream", kind: "text", required: true, hint: "The upstream's name." },
  { path: "auth_mode", label: "Auth mode", kind: "select", options: AUTH_MODES, hint: "Unset means none (GW-20)." },
  { path: "priority", label: "Priority", kind: "int", hint: "Breaks ties between routes that match alike (GW-01)." },
  { path: "cost", label: "Cost", kind: "int", hint: "Units each request takes from rate limits and quotas; unset means 1." },
  { path: "match.host", label: "Host", kind: "text", group: "Match" },
  { path: "match.path", label: "Path", kind: "text", group: "Match", hint: "Exact, with :param segments. Set this or the prefix." },
  { path: "match.path_prefix", label: "Path prefix", kind: "text", group: "Match" },
  { path: "match.methods", label: "Methods", kind: "list", group: "Match", hint: "Comma-separated, such as GET, POST; empty for every method." },
  { path: "match.headers", label: "Header predicates", kind: "headerMatch", group: "Match", hint: "One a line: Name: value (exact) or Name ~ regex." },
  { path: "strip_prefix", label: "Strip prefix", kind: "text", group: "Transforms" },
  { path: "add_prefix", label: "Add prefix", kind: "text", group: "Transforms" },
  { path: "request_headers.set", label: "Request headers to set", kind: "headers", group: "Transforms", hint: "One a line: Name: value." },
  { path: "request_headers.add", label: "Request headers to add", kind: "headers", group: "Transforms", hint: "One a line: Name: value." },
  { path: "request_headers.remove", label: "Request headers to remove", kind: "list", group: "Transforms", hint: "Names, comma-separated." },
  { path: "response_headers.set", label: "Response headers to set", kind: "headers", group: "Transforms", hint: "One a line: Name: value." },
  { path: "response_headers.add", label: "Response headers to add", kind: "headers", group: "Transforms", hint: "One a line: Name: value." },
  { path: "response_headers.remove", label: "Response headers to remove", kind: "list", group: "Transforms", hint: "Names, comma-separated." },
  { path: "timeouts.connect", label: "Connect timeout", kind: "text", group: "Timeouts and retries", hint: DURATION },
  { path: "timeouts.per_try", label: "Per-try timeout", kind: "text", group: "Timeouts and retries", hint: DURATION },
  { path: "timeouts.total", label: "Total timeout", kind: "text", group: "Timeouts and retries", hint: DURATION },
  { path: "timeouts.idle", label: "Idle timeout", kind: "text", group: "Timeouts and retries", hint: DURATION },
  { path: "retry.attempts", label: "Retry attempts", kind: "int", group: "Timeouts and retries" },
  { path: "retry.non_idempotent", label: "Retry non-idempotent methods too", kind: "bool", group: "Timeouts and retries" },
  { path: "max_body_bytes", label: "Max body bytes", kind: "int", group: "Limits" },
  { path: "max_header_bytes", label: "Max header bytes", kind: "int", group: "Limits" },
  { path: "hsts_max_age", label: "HSTS max age", kind: "text", group: "Limits", hint: DURATION },
];

export const UPSTREAM_FIELDS: readonly FieldSpec[] = [
  { path: "name", label: "Name", kind: "text", required: true },
  { path: "endpoints", label: "Endpoints", kind: "endpoints", required: true, hint: "One host:port a line. A kept address keeps its draining state." },
  { path: "lb", label: "Load balancing", kind: "select", options: LB, hint: "Unset means p2c (GW-07)." },
  { path: "hash_on", label: "Hash on", kind: "text", hint: "consistent_hash's key template." },
  { path: "health_check.path", label: "Health check path", kind: "text", group: "Health checks" },
  { path: "health_check.interval", label: "Health check interval", kind: "text", group: "Health checks", hint: DURATION },
  { path: "health_check.timeout", label: "Health check timeout", kind: "text", group: "Health checks", hint: DURATION },
  { path: "health_check.unhealthy", label: "Unhealthy after", kind: "int", group: "Health checks", hint: "Failed probes in a row." },
  { path: "health_check.healthy", label: "Healthy after", kind: "int", group: "Health checks", hint: "Passed probes in a row." },
  { path: "outlier_detection.consecutive_failures", label: "Consecutive failures", kind: "int", group: "Outlier detection" },
  { path: "outlier_detection.base_ejection", label: "Base ejection", kind: "text", group: "Outlier detection", hint: DURATION },
  { path: "outlier_detection.max_ejection", label: "Max ejection", kind: "text", group: "Outlier detection", hint: DURATION },
  { path: "outlier_detection.max_ejected_percent", label: "Max ejected percent", kind: "int", group: "Outlier detection" },
  { path: "circuit.max_connections", label: "Max connections", kind: "int", group: "Circuit breaker" },
  { path: "circuit.max_pending", label: "Max pending", kind: "int", group: "Circuit breaker" },
  { path: "circuit.max_concurrent", label: "Max concurrent", kind: "int", group: "Circuit breaker" },
  { path: "circuit.error_rate", label: "Error rate", kind: "number", group: "Circuit breaker", hint: "A share, such as 0.5." },
  { path: "circuit.window", label: "Window", kind: "text", group: "Circuit breaker", hint: DURATION },
  { path: "circuit.min_requests", label: "Min requests", kind: "int", group: "Circuit breaker" },
  { path: "circuit.open_for", label: "Open for", kind: "text", group: "Circuit breaker", hint: DURATION },
  { path: "circuit.half_open_probes", label: "Half-open probes", kind: "int", group: "Circuit breaker" },
  { path: "retry_budget.ratio", label: "Retry ratio", kind: "number", group: "Retry budget", hint: "A share of requests, such as 0.2." },
  { path: "retry_budget.min_per_sec", label: "Min retries per second", kind: "number", group: "Retry budget" },
];

/** The fields the API sets, never the client. */
const READ_ONLY = ["id", "version", "created_at", "updated_at"];

type Obj = Record<string, unknown>;

/** The entity's writable settings: what the API answered without its read-only fields. */
export function writable<T extends object>(e: T): Obj {
  return Object.fromEntries(Object.entries(e).filter(([k]) => !READ_ONLY.includes(k)));
}

function get(o: unknown, path: string): unknown {
  return path.split(".").reduce<unknown>((v, k) => (typeof v === "object" && v !== null ? (v as Obj)[k] : undefined), o);
}

const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v);

/** o with value at the path keys (undefined: without it), objects made on the way and empty ones left behind dropped. */
function put(o: Obj, keys: readonly string[], value: unknown): Obj {
  const [k, ...rest] = keys;
  if (k === undefined) return o;
  const others = Object.fromEntries(Object.entries(o).filter(([key]) => key !== k));
  const old = o[k];
  const next = rest.length === 0 ? value : put(isObj(old) ? old : {}, rest, value);
  if (next === undefined || (isObj(next) && rest.length > 0 && Object.keys(next).length === 0)) return others;
  return { ...others, [k]: next };
}

const str = (v: unknown) => (typeof v === "string" || typeof v === "number" ? String(v) : "");
const items = (v: unknown): Obj[] => (Array.isArray(v) ? v.filter((x): x is Obj => typeof x === "object" && x !== null) : []);

/** One field's value as the form shows it. */
function show(spec: FieldSpec, v: unknown): string | boolean {
  switch (spec.kind) {
    case "bool":
      return v === true;
    case "list":
      return Array.isArray(v) ? v.map(str).join(", ") : "";
    case "headers":
      return items(v).map((h) => `${str(h.name)}: ${str(h.value)}`).join("\n");
    case "headerMatch":
      return items(v).map((h) => (h.regex ? `${str(h.name)} ~ ${str(h.regex)}` : `${str(h.name)}: ${str(h.exact)}`)).join("\n");
    case "endpoints":
      return items(v).map((e) => str(e.address)).join("\n");
    default:
      return str(v);
  }
}

/** The form of entity e (none: a new one) under specs. */
export function valuesOf(specs: readonly FieldSpec[], e?: object): Values {
  return Object.fromEntries(specs.map((s) => [s.path, show(s, e && get(e, s.path))]));
}

const lines = (s: string) => s.split("\n").map((l) => l.trim()).filter(Boolean);
const commaList = (s: string) => s.split(",").map((x) => x.trim()).filter(Boolean);

/** A field's setting from its value, or the reason it has none; undefined leaves it unset. */
function parse(spec: FieldSpec, raw: string | boolean, before: unknown): { value?: unknown; error?: string } {
  if (spec.kind === "bool") return { value: raw === true ? true : undefined };
  const text = String(raw).trim();
  if (text === "") return spec.required ? { error: "Required" } : {};
  switch (spec.kind) {
    case "int": {
      if (!/^-?\d+$/.test(text) || !Number.isSafeInteger(Number(text))) return { error: "Must be a whole number" };
      return { value: Number(text) };
    }
    case "number": {
      const n = Number(text);
      return Number.isFinite(n) ? { value: n } : { error: "Must be a number" };
    }
    case "list":
      return { value: commaList(text) };
    case "headers": {
      const out = [];
      for (const l of lines(text)) {
        const i = l.indexOf(":");
        if (i <= 0) return { error: `"${l}": want Name: value` };
        out.push({ name: l.slice(0, i).trim(), value: l.slice(i + 1).trim() });
      }
      return { value: out };
    }
    case "headerMatch": {
      const out = [];
      for (const l of lines(text)) {
        const re = /^([^:~\s]+)\s*~\s*(.+)$/.exec(l);
        const exact = /^([^:~\s]+)\s*:\s*(.*)$/.exec(l);
        if (re) out.push({ name: re[1], regex: re[2] });
        else if (exact) out.push({ name: exact[1], exact: exact[2] });
        else return { error: `"${l}": want Name: value or Name ~ regex` };
      }
      return { value: out };
    }
    case "endpoints": {
      // A kept address keeps its draining state; a new one takes traffic.
      const draining = new Map(items(before).map((e) => [str(e.address), e.draining === true]));
      return { value: lines(text).map((address) => (draining.get(address) ? { address, draining: true } : { address })) };
    }
    default:
      if (spec.path === "name" && !NAME_RE.test(text)) return { error: NAME_RULE };
      return { value: text };
  }
}

/**
 * The body a save sends: base (the entity's writable settings, or {} for a
 * new one) with every field's setting at its path, or the fields'
 * errors when any value does not parse.
 */
export function bodyOf(specs: readonly FieldSpec[], values: Values, base: Obj = {}): { body: Obj; errors: FieldErrors } {
  let body = base;
  const errors: FieldErrors = {};
  for (const s of specs) {
    const r = parse(s, values[s.path] ?? "", get(base, s.path));
    if (r.error) errors[s.path] = r.error;
    else body = put(body, s.path.split("."), r.value);
  }
  return { body, errors };
}

/** The field a server message is about: the longest field path, or its top-level name, it starts with. */
function fieldOfMessage(specs: readonly FieldSpec[], msg: string): string | undefined {
  let best: string | undefined;
  for (const s of specs) {
    const candidates = [s.path, s.kind === "endpoints" ? "endpoint " : ""].filter(Boolean);
    for (const c of candidates) {
      const next = msg.charAt(c.length);
      if (msg.startsWith(c) && (c.endsWith(" ") || next === "" || !/[\w]/.test(next)) && (!best || s.path.length > best.length)) best = s.path;
    }
  }
  return best;
}

/** A 422's details on the fields they name, by path or by the message's start; the rest as a list. */
export function serverErrors(specs: readonly FieldSpec[], details: readonly ErrorDetail[]): { fields: FieldErrors; rest: string[] } {
  const fields: FieldErrors = {};
  const rest: string[] = [];
  for (const d of details) {
    const msg = d.message ?? "";
    const field = d.path && specs.some((s) => s.path === d.path) ? d.path : fieldOfMessage(specs, msg);
    if (field && !fields[field]) fields[field] = msg;
    else rest.push(d.path ? `${d.path}: ${msg}` : msg);
  }
  return { fields, rest };
}
