// Fails when src/api/schema.gen.ts is not what api/openapi.yaml generates
// (UI-05, ADR-0057), as make lint-api does for the Go stubs.
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const dir = mkdtempSync(join(tmpdir(), "spillway-gen-"));
try {
  const out = join(dir, "schema.gen.ts");
  execFileSync("openapi-typescript", ["../api/openapi.yaml", "-o", out], { stdio: ["ignore", "ignore", "inherit"] });
  if (readFileSync(out, "utf8") !== readFileSync("src/api/schema.gen.ts", "utf8")) {
    console.error("web/src/api/*.gen.ts is stale: run make web-gen");
    process.exitCode = 1;
  }
} finally {
  rmSync(dir, { recursive: true, force: true });
}
