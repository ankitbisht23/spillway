import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { createQueryClient } from "../../api/queryClient";
import type { Role } from "../../auth/permissions";
import { setSession } from "../../auth/session";
import { startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { graceError, isLive } from "./Keys";
import { apiKey, cached, mockConsumer, ROTATED, SECRET } from "./testing";

beforeEach(() => startSession());
afterEach(() => setSession(null));

async function open(roles: Role[] = ["key-admin"]) {
  const qc = createQueryClient();
  const router = renderApp("/consumers/7", roles, () => {}, qc);
  await screen.findByRole("heading", { name: "Consumer checkout" });
  await screen.findByRole("row", { name: /spw_Zz99Yy88_/ });
  return { qc, router };
}

const keys = () => within(screen.getByRole("region", { name: "API keys" }));

/** The secret's own part, which no answer but issue and rotate ever has. */
const tail = (k: string) => k.split("_")[2] ?? "";

describe("API keys (CP-15 – CP-17)", () => {
  it("lists prefix, created, expiry and revocation, with actions only on live keys", async () => {
    mockConsumer({ keys: [apiKey(), apiKey({ id: 23, prefix: "Rr11Rr11", revoked_at: "2026-11-16T10:05:00Z" }), apiKey({ id: 24, prefix: "Ee22Ee22", expires_at: "2020-01-01T00:00:00Z" })] });
    await open();
    expect(keys().getByRole("row", { name: /spw_Zz99Yy88_/ })).toHaveTextContent("Never");
    expect(keys().getByRole("button", { name: "Rotate spw_Zz99Yy88_…" })).toBeEnabled();
    const revoked = within(keys().getByRole("row", { name: /spw_Rr11Rr11_/ }));
    expect(revoked.queryByRole("button")).not.toBeInTheDocument();
    expect(revoked.getByText("Revoked")).toBeInTheDocument();
    const expired = within(keys().getByRole("row", { name: /spw_Ee22Ee22_/ }));
    expect(expired.queryByRole("button")).not.toBeInTheDocument();
    expect(expired.getAllByText(/expired/i).length).toBeGreaterThan(0);
  });

  it("issues a key, shows it once with copy, and keeps it out of every cache, storage, URL and the console", async () => {
    const api = mockConsumer();
    const user = userEvent.setup();
    const log = [vi.spyOn(console, "log"), vi.spyOn(console, "info"), vi.spyOn(console, "debug")];
    const { qc, router } = await open();

    await user.click(keys().getByRole("button", { name: "Issue key" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Issue an API key for checkout" }));
    await user.click(dialog.getByRole("button", { name: "Issue key" }));
    expect(await dialog.findByLabelText("The new API key")).toHaveTextContent(SECRET);
    expect(api.sent).toEqual([{ method: "POST", path: "/api/v1/consumers/7/keys", ifMatch: null, body: { expires_at: null } }]);

    // While it is shown, it is in the dialog's state only.
    expect(cached(qc)).not.toContain(tail(SECRET));
    expect(cached(qc)).not.toContain("spw_");
    await user.click(dialog.getByRole("button", { name: "Copy key" }));
    expect(await dialog.findByText("Copied.")).toBeInTheDocument();
    expect(await navigator.clipboard.readText()).toBe(SECRET);

    await user.click(dialog.getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain(tail(SECRET));
    expect(await keys().findByRole("row", { name: /spw_Ab12Cd34_/ })).toBeInTheDocument();
    // The issue mutation is gone from the cache once its dialog closed (gcTime 0).
    await waitFor(() => expect(qc.getMutationCache().getAll()).toHaveLength(0));
    expect(cached(qc)).not.toContain(tail(SECRET));
    expect(cached(qc)).not.toContain("spw_");
    expect(JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage })).not.toContain("spw_");
    expect(router.state.location.href).not.toContain(tail(SECRET));
    for (const spy of log) expect(JSON.stringify(spy.mock.calls)).not.toContain(tail(SECRET));
  });

  it("rotates with a grace period and a note, shows the new key once, and keeps it out of the caches", async () => {
    const api = mockConsumer();
    const user = userEvent.setup();
    const { qc } = await open(["admin"]);
    await user.click(keys().getByRole("button", { name: "Rotate spw_Zz99Yy88_…" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Rotate key spw_Zz99Yy88_…" }));
    const grace = dialog.getByRole("textbox", { name: "Grace period" });
    expect(grace).toHaveValue("24h");
    await user.clear(grace);
    await user.type(grace, "1h30m");
    await user.type(dialog.getByRole("textbox", { name: /^Note/ }), "quarterly");
    await user.click(dialog.getByRole("button", { name: "Rotate key" }));
    expect(await dialog.findByLabelText("The new API key")).toHaveTextContent(ROTATED);
    expect(dialog.getByText(/The old key 20 keeps working until/)).toBeInTheDocument();
    expect(api.sent).toEqual([{ method: "POST", path: "/api/v1/keys/20/rotate", ifMatch: null, body: { grace: "1h30m", note: "quarterly" } }]);
    expect(cached(qc)).not.toContain(tail(ROTATED));

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain(tail(ROTATED));
    expect(await keys().findByRole("row", { name: /spw_Ef56Gh78_/ })).toHaveTextContent("Key 20");
    await waitFor(() => expect(qc.getMutationCache().getAll()).toHaveLength(0));
    expect(cached(qc)).not.toContain(tail(ROTATED));
    expect(cached(qc)).not.toContain("spw_");
  });

  it("checks the grace period in the browser: 0s to 168h", async () => {
    const api = mockConsumer();
    const user = userEvent.setup();
    await open();
    await user.click(keys().getByRole("button", { name: "Rotate spw_Zz99Yy88_…" }));
    const dialog = within(await screen.findByRole("dialog"));
    const grace = dialog.getByRole("textbox", { name: "Grace period" });
    await user.clear(grace);
    await user.type(grace, "169h");
    await user.click(dialog.getByRole("button", { name: "Rotate key" }));
    expect(dialog.getByText("At most 168h (7 days)")).toBeInTheDocument();
    expect(grace).toHaveAttribute("aria-invalid", "true");
    expect(api.sent).toEqual([]);
  });

  it("shows the server's 409 when the key cannot be rotated", async () => {
    mockConsumer({ answer: () => HttpResponse.json({ code: "conflict", error: "the key is revoked or expired; issue a new key instead" }, { status: 409 }) });
    const user = userEvent.setup();
    await open();
    await user.click(keys().getByRole("button", { name: "Rotate spw_Zz99Yy88_…" }));
    const dialog = within(await screen.findByRole("dialog"));
    await user.click(dialog.getByRole("button", { name: "Rotate key" }));
    expect(await dialog.findByRole("alert")).toHaveTextContent("Conflict: the key is revoked or expired; issue a new key instead");
  });

  it("revokes only after confirming, with the note", async () => {
    const api = mockConsumer();
    const user = userEvent.setup();
    await open();
    await user.click(keys().getByRole("button", { name: "Revoke spw_Zz99Yy88_…" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Revoke key spw_Zz99Yy88_…?" }));
    expect(dialog.getByText(/refuses it with 401 within 5 s/)).toBeInTheDocument();
    expect(api.sent).toEqual([]);
    await user.type(dialog.getByRole("textbox", { name: /^Note/ }), "leaked");
    await user.click(dialog.getByRole("button", { name: "Revoke key" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(api.sent).toEqual([{ method: "POST", path: "/api/v1/keys/20/revoke", ifMatch: null, body: { note: "leaked" } }]);
    expect(await within(keys().getByRole("row", { name: /spw_Zz99Yy88_/ })).findByText("Revoked")).toBeInTheDocument();
  });

  it("blocks a key with a deny entry on it", async () => {
    mockConsumer();
    const user = userEvent.setup();
    await open();
    await user.click(keys().getByRole("button", { name: "Block spw_Zz99Yy88_…" }));
    const dialog = within(await screen.findByRole("dialog", { name: "New access-list entry" }));
    expect(dialog.getByRole("combobox", { name: "Action (required)" })).toHaveValue("deny");
    expect(dialog.getByRole("combobox", { name: "Subject type (required)" })).toHaveValue("key");
    expect(dialog.getByRole("textbox", { name: "API key ID" })).toHaveValue("20");
  });

  it.each<[Role, boolean]>([["viewer", false], ["editor", false], ["publisher", false], ["key-admin", true], ["admin", true]])(
    "for %s: issue, rotate, revoke and block are allowed: %s, else disabled with the reason",
    async (role, allowed) => {
      mockConsumer();
      await open([role]);
      for (const name of ["Issue key", "Rotate spw_Zz99Yy88_…", "Revoke spw_Zz99Yy88_…", "Block spw_Zz99Yy88_…"]) {
        const b = keys().getByRole("button", { name });
        if (allowed) {
          expect(b, name).toBeEnabled();
        } else {
          expect(b, name).toBeDisabled();
          expect(b, name).toHaveAccessibleDescription("Needs the key-admin or admin role");
        }
      }
    },
  );

  it("shows the empty state with the reason a viewer cannot issue", async () => {
    mockConsumer({ keys: [] });
    renderApp("/consumers/7", ["viewer"]);
    expect(await screen.findByText("No API keys yet. Needs the key-admin or admin role to issue one.")).toBeInTheDocument();
  });
});

describe("a rotation's grace period and a key's state", () => {
  it.each([["24h", undefined], ["0", undefined], ["0s", undefined], ["90m", undefined], ["1h30m15s", undefined], ["168h", undefined],
    ["168h1s", "At most 168h (7 days)"], ["8d", "Want a duration from 0s to 168h, such as 24h or 90m"], ["", "Want a duration from 0s to 168h, such as 24h or 90m"],
    ["-1h", "Want a duration from 0s to 168h, such as 24h or 90m"]])("%s: %s", (g, want) => {
    expect(graceError(g)).toBe(want);
  });

  it("a key is live until revoked or past its expiry", () => {
    const now = Date.parse("2026-11-16T12:00:00Z");
    expect(isLive(apiKey(), now)).toBe(true);
    expect(isLive(apiKey({ expires_at: "2026-11-16T13:00:00Z" }), now)).toBe(true);
    expect(isLive(apiKey({ expires_at: "2026-11-16T11:00:00Z" }), now)).toBe(false);
    expect(isLive(apiKey({ revoked_at: "2026-11-16T11:00:00Z" }), now)).toBe(false);
  });
});
