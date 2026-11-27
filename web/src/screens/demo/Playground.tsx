// The Playground (UI-07, SC-04), shown only in demo mode (SC-08, ADR-0095
// §2): its URL is Not found unless GET /api/v1/demo answers. For now it
// shows the demo's tenants and routes with their limits; the Playground
// task adds its traffic.
import type { components } from "../../api/schema.gen";
import { Loading, NotFound } from "../../components/states";
import { useDemo } from "./useDemo";

type Limit = components["schemas"]["DemoLimit"];

function limitText(l: Limit | null): string {
  return l ? `${l.rate} per ${l.period}, burst ${l.burst}` : "each tenant's plan";
}

export function Playground() {
  const demo = useDemo();
  if (demo.status === "pending") return <Loading />;
  if (demo.status === "error") return <NotFound />;
  const { tenants, routes } = demo.data;
  return (
    <section>
      <h1>Playground</h1>
      <p className="muted">Demo mode is on: every API key issued here expires within 30 minutes.</p>
      <h2>Tenants</h2>
      <table>
        <thead>
          <tr><th>Tenant</th><th>Plan</th><th>Limit</th></tr>
        </thead>
        <tbody>
          {tenants.map((t) => (
            <tr key={t.name}><td>{t.name}</td><td>{t.plan ?? "none"}</td><td>{t.limit ? limitText(t.limit) : "none"}</td></tr>
          ))}
        </tbody>
      </table>
      <h2>Routes</h2>
      <table>
        <thead>
          <tr><th>Route</th><th>Policy</th><th>Limit</th></tr>
        </thead>
        <tbody>
          {routes.map((r) => (
            <tr key={r.name}><td>{r.name}</td><td>{r.policy ?? "none"}</td><td>{r.policy ? limitText(r.limit) : "none"}</td></tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
