// Fails when src/api/schema.gen.ts or src/api/roles.gen.ts is not what
// api/openapi.yaml generates (UI-05, ADR-0057, ADR-0058), as make lint-api
// does for the Go stubs.
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const dir = mkdtempSync(join(tmpdir(), "spillway-gen-"));
try {
  const schema = join(dir, "schema.gen.ts");
  const roles = join(dir, "roles.gen.ts");
  execFileSync("openapi-typescript", ["../api/openapi.yaml", "-o", schema], { stdio: ["ignore", "ignore", "inherit"] });
  execFileSync(process.execPath, ["scripts/gen-roles.mjs", roles], { stdio: ["ignore", "ignore", "inherit"] });
  /** @type {[string, string][]} */
  const pairs = [[schema, "src/api/schema.gen.ts"], [roles, "src/api/roles.gen.ts"]];
  const stale = pairs.some(([got, committed]) => readFileSync(got, "utf8") !== readFileSync(committed, "utf8"));
  if (stale) {
    console.error("web/src/api/*.gen.ts is stale: run make web-gen");
    process.exitCode = 1;
  }
} finally {
  rmSync(dir, { recursive: true, force: true });
}
