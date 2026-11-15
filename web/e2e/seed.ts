// What every Playwright run starts from (ADR-0061), through the REST API
// as the spillway-ci client: e2e/fixtures/e2e.yaml imported with If-Match
// and published, and a tenant, consumer and API key named e2e-<run id>.
// The Throttling screen also needs a throttled key, which only a managed
// gateway's heartbeat reports and make dev's gateways are static: one
// heavy-hitter window is inserted into compose's Postgres for the run's
// key. Teardown removes every e2e- tenant with its consumers, their keys'
// access-list entries and keys, and the inserted windows, so a killed run
// is cleaned by the next one. No token or key is ever printed.
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { request, type APIRequestContext, type APIResponse } from "@playwright/test";

import { APP, DEV_PASSWORD, TOKEN_ENDPOINT } from "./env";

export const RUN_PREFIX = "e2e-";
const REPO = fileURLToPath(new URL("../..", import.meta.url));
const FIXTURE = fileURLToPath(new URL("fixtures/e2e.yaml", import.meta.url));

/** What setup made, for the specs (through the environment, which workers inherit). */
export interface Seeded {
  run: string;
  policyId: number;
  consumerId: number;
  keyId: number;
  digest: string;
}

export function seeded(): Seeded {
  const s = process.env.SPILLWAY_E2E_SEEDED;
  if (!s) throw new Error("global setup did not run");
  return JSON.parse(s) as Seeded;
}

/** An API client of the control plane with a spillway-ci token. */
async function ciApi(): Promise<APIRequestContext> {
  const kc = await request.newContext();
  const r = await kc.post(TOKEN_ENDPOINT, {
    form: { grant_type: "client_credentials", client_id: "spillway-ci", client_secret: process.env.SPILLWAY_CI_CLIENT_SECRET || DEV_PASSWORD },
  });
  if (!r.ok()) throw new Error(`spillway-ci token: status ${r.status()}`);
  const { access_token: token } = (await r.json()) as { access_token: string };
  await kc.dispose();
  return request.newContext({ baseURL: `${APP}/api/v1/`, extraHTTPHeaders: { Authorization: `Bearer ${token}` } });
}

/** r's JSON body if its status is one of want; else an error naming the call, the status and the API's message. */
async function must<T>(what: string, r: Promise<APIResponse>, ...want: number[]): Promise<T> {
  const res = await r;
  if (!want.includes(res.status())) {
    let msg = "";
    try {
      msg = ((await res.json()) as { error?: string }).error ?? "";
    } catch {
      // not JSON
    }
    throw new Error(`${what}: status ${res.status()} ${msg}`);
  }
  return res.status() === 204 ? (undefined as T) : ((await res.json()) as T);
}

const etag = (version: number) => ({ "If-Match": `"${version}"` });

interface Page<T> {
  items: T[];
}
interface Versioned {
  id: number;
  version: number;
}

/** psql in compose's Postgres as its owner role; only integers and generated digests reach sql. */
function psql(sql: string) {
  execFileSync("docker", ["compose", "exec", "-T", "postgres", "psql", "-q", "-U", "spillway", "-d", "spillway", "-v", "ON_ERROR_STOP=1", "-c", sql], {
    cwd: REPO, stdio: ["ignore", "ignore", "inherit"],
  });
}

/** Removes every e2e- tenant with what it holds, and the inserted heavy-hitter windows. */
async function removeRuns(api: APIRequestContext) {
  const tenants = await must<Page<Versioned & { name: string }>>("list tenants", api.get("tenants?limit=500"), 200);
  for (const t of tenants.items.filter((x) => x.name.startsWith(RUN_PREFIX))) {
    const consumers = await must<Page<Versioned>>("list consumers", api.get(`consumers?tenant_id=${t.id}&limit=500`), 200);
    for (const c of consumers.items) {
      const keys = await must<Page<Versioned & { revoked_at: string | null }>>("list keys", api.get(`consumers/${c.id}/keys?limit=500`), 200);
      const ids = new Set(keys.items.map((k) => k.id));
      const entries = await must<Page<Versioned & { api_key_id: number | null }>>("list access lists", api.get("access-lists?subject_type=key&limit=500"), 200);
      for (const e of entries.items.filter((x) => x.api_key_id !== null && ids.has(x.api_key_id))) {
        await must("delete access-list entry", api.delete(`access-lists/${e.id}`, { headers: etag(e.version) }), 204);
      }
      for (const k of keys.items.filter((x) => x.revoked_at === null)) {
        await must("revoke key", api.post(`keys/${k.id}/revoke`, { data: { note: "Playwright teardown" } }), 200);
      }
      const now = await must<Versioned>("get consumer", api.get(`consumers/${c.id}`), 200);
      await must("delete consumer", api.delete(`consumers/${c.id}`, { headers: etag(now.version) }), 204);
    }
    await must("delete tenant", api.delete(`tenants/${t.id}`, { headers: etag(t.version) }), 204);
  }
  psql(`DELETE FROM heavy_hitters WHERE key_digest LIKE '${RUN_PREFIX}%'`);
}

async function idOf(api: APIRequestContext, kind: string, name: string): Promise<number> {
  const page = await must<Page<{ id: number }>>(`find ${kind} ${name}`, api.get(`${kind}?name=${name}`), 200);
  const id = page.items[0]?.id;
  if (id === undefined) throw new Error(`${kind} ${name} is not in the working set`);
  return id;
}

export async function setup() {
  const api = await ciApi();
  try {
    await removeRuns(api);
    const run = `${RUN_PREFIX}${Date.now().toString(36)}`;
    const note = `Playwright ${run}: web/e2e/fixtures/e2e.yaml`;
    const exported = await api.get("config/export");
    if (exported.status() !== 200) throw new Error(`export: status ${exported.status()}`);
    await must("import", api.post(`config/import?note=${encodeURIComponent(note)}`, {
      headers: { "If-Match": exported.headers().etag ?? "", "Content-Type": "application/yaml" }, data: readFileSync(FIXTURE, "utf8"),
    }), 200);
    const validated = await api.post("config/validate");
    const v = (await validated.json()) as { valid: boolean };
    if (validated.status() !== 200 || !v.valid) throw new Error(`validate: status ${validated.status()}, valid ${v.valid}`);
    // 409: the working set already is that config, published.
    await must("publish", api.post("config/publish", { headers: { "If-Match": validated.headers().etag ?? "" }, data: { note } }), 200, 409);

    const policyId = await idOf(api, "policies", "e2e-per-client");
    const planId = await idOf(api, "plans", "e2e");
    const tenant = await must<{ id: number }>("create tenant", api.post("tenants", { data: { name: run, plan_id: planId } }), 201);
    const consumer = await must<{ id: number }>("create consumer", api.post("consumers", { data: { tenant_id: tenant.id, name: run } }), 201);
    const key = await must<{ id: number }>("issue key", api.post(`consumers/${consumer.id}/keys`, { data: {} }), 201);
    const digest = (run + randomBytes(16).toString("base64url")).slice(0, 22);
    psql(`INSERT INTO heavy_hitters (window_start, policy_id, key_digest, consumer_id, api_key_id, denied, shadow_denied)
      VALUES (date_trunc('minute', now()), ${policyId}, '${digest}', ${consumer.id}, ${key.id}, 42, 0)`);
    const s: Seeded = { run, policyId, consumerId: consumer.id, keyId: key.id, digest };
    process.env.SPILLWAY_E2E_SEEDED = JSON.stringify(s);
  } finally {
    await api.dispose();
  }
}

export async function teardown() {
  const api = await ciApi();
  try {
    await removeRuns(api);
  } finally {
    await api.dispose();
  }
}
