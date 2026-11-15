import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["src/api/*.gen.ts", "playwright-report/", "test-results/", "coverage/"] },
  js.configs.recommended,
  tseslint.configs.strict,
  reactHooks.configs.flat.recommended,
  // Nothing logs in the browser: a logged request or response could carry a
  // token or a key (ADR-0058).
  { rules: { "no-console": "error" } },
  { files: ["**/*.test.{ts,tsx}", "src/test/**", "scripts/**", "e2e/**"], rules: { "no-console": "off" } },
  { files: ["scripts/**"], languageOptions: { globals: { console: "readonly", process: "readonly" } } },
);
