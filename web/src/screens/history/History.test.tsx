import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import type { components } from "../../api/schema.gen";
import type { Role } from "../../auth/permissions";
import { setSession } from "../../auth/session";
import { startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";
import { discardWarning } from "./RollbackDialog";

type Version = components["schemas"]["ConfigVersion"];
type Validation = components["schemas"]["ConfigValidation"];

const HOSTILE = `<img src=x onerror="alert(1)">`;

function version(v: number, over: Partial<Version> = {}): Version {
  return {
    version: v, published_at: "2026-11-16T10:00:00Z", published_by: "sub-pub", note: `note ${v}`, kind: "publish", rollback_of: null,
    working_set_revision: v * 2, schema_version: { major: 1, minor: 0 }, ...over,
  };
}

const diffOf = (from: number, to: number) => ({
  from_version: from, to_version: to,
  changes: [{ entity: "policy", id: 9, name: "per-client", change: "removed" }],
  diff: `--- version ${from}\n+++ version ${to}\n@@ -1,2 +1 @@\n-  name: per-client\n ok\n`,
});

/** Serves two pages of versions: cursor "MTA" is the second. */
function mockList() {
  const asked: (string | null)[] = [];
  server.use(http.get("/api/v1/config/versions", ({ request }) => {
    const u = new URL(request.url);
    asked.push(u.searchParams.get("cursor"));
    expect(u.searchParams.get("limit")).toBe("50");
    return u.searchParams.get("cursor") === "MTA"
      ? HttpResponse.json({ items: [version(9)], next_cursor: null })
      : HttpResponse.json({ items: [version(12, { kind: "rollback", rollback_of: 10, note: HOSTILE }), version(11), version(10)], next_cursor: "MTA" });
  }));
  return asked;
}

/** Serves version v and its diff, recording each diff's against. */
function mockVersion(v: number, over: Partial<Version> = {}) {
  const against: (string | null)[] = [];
  server.use(
    http.get(`/api/v1/config/versions/${v}`, () => HttpResponse.json({ ...version(v, over), snapshot: {}, applied: [{ replica: "gateway-1", at: "2026-11-16T10:00:01Z" }] })),
    http.get(`/api/v1/config/versions/${v}/diff`, ({ request }) => {
      const a = new URL(request.url).searchParams.get("against");
      against.push(a);
      return HttpResponse.json(diffOf(a === null ? v - 1 : Number(a), v));
    }),
  );
  return against;
}

function preview(revision: number, over: Partial<Validation> = {}): Validation {
  return {
    working_set_revision: revision, base_version: 12, valid: true, errors: [],
    changes: [{ entity: "route", id: 1, name: "orders", change: "changed" }, { entity: "policy", id: 2, name: "p", change: "added" }],
    diff: "--- version 12\n+++ working set\n@@ -1 +1 @@\n-a\n+b\n", ...over,
  };
}

/** Serves validate with each preview in turn (the last again after). */
function mockValidate(...previews: Validation[]) {
  const calls: number[] = [];
  server.use(http.post("/api/v1/config/validate", () => {
    const p = previews[Math.min(calls.length, previews.length - 1)] as Validation;
    calls.push(p.working_set_revision);
    return HttpResponse.json(p, { headers: { ETag: `"${p.working_set_revision}"` } });
  }));
  return calls;
}

/** Serves rollback of version v with each answer in turn, recording what was sent. */
function mockRollback(v: number, ...answers: (() => Response)[]) {
  const sent: { ifMatch: string | null; body: unknown }[] = [];
  server.use(http.post(`/api/v1/config/versions/${v}/rollback`, async ({ request }) => {
    sent.push({ ifMatch: request.headers.get("If-Match"), body: await request.json() });
    const a = answers[Math.min(sent.length - 1, answers.length - 1)];
    return a ? a() : HttpResponse.json({}, { status: 500 });
  }));
  return sent;
}

const apiError = (status: number, code: string, error: string, details?: unknown[]) => () =>
  HttpResponse.json({ error, code, request_id: "req-1", details }, { status });

beforeEach(() => {
  startSession();
  server.use(http.get("/api/v1/replicas", () => HttpResponse.json({ items: [], next_cursor: null })));
});
afterEach(() => setSession(null));

describe("History: the version list (UI-06, D9)", () => {
  it("lists 50 a page, newest first, with kind, publisher and note, and pages by cursor", async () => {
    const asked = mockList();
    const router = renderApp("/history", ["viewer"]);
    const row = await screen.findByRole("row", { name: /^Version 12/ });
    expect(row).toHaveTextContent("rollback to version 10");
    expect(within(row).getByRole("link", { name: "version 10" })).toHaveAttribute("href", "/history/10");
    expect(within(row).getByText(HOSTILE)).toBeInTheDocument();
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getByRole("row", { name: /^Version 11/ })).toHaveTextContent("publish");
    expect(screen.getByRole("link", { name: "Version 11" })).toHaveAttribute("href", "/history/11");
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(await screen.findByRole("row", { name: /^Version 9/ })).toBeInTheDocument();
    expect(router.state.location.search).toEqual({ cursor: "MTA" });
    expect(asked).toEqual([null, "MTA"]);
  });

  it("links the audit log for admins only", async () => {
    mockList();
    renderApp("/history", ["admin"]);
    expect(await screen.findByRole("link", { name: "audit log" })).toHaveAttribute("href", "/audit");
  });

  it("does not mention the audit log to other roles", async () => {
    mockList();
    renderApp("/history", ["publisher"]);
    await screen.findByRole("row", { name: /^Version 12/ });
    expect(screen.queryByRole("link", { name: "audit log" })).toBeNull();
  });

  it("shows the empty state before the first publish", async () => {
    server.use(http.get("/api/v1/config/versions", () => HttpResponse.json({ items: [], next_cursor: null })));
    renderApp("/history", ["viewer"]);
    expect(await screen.findByText("No config version is published yet.")).toBeInTheDocument();
  });

  it("shows the error state with Retry", async () => {
    let fail = true;
    server.use(http.get("/api/v1/config/versions", () =>
      fail ? apiError(500, "internal", "boom")() : HttpResponse.json({ items: [version(1)], next_cursor: null })));
    renderApp("/history", ["viewer"]);
    expect(await screen.findByRole("alert")).toHaveTextContent("Something went wrong");
    expect(screen.getByText("req-1")).toBeInTheDocument();
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("row", { name: /^Version 1/ })).toBeInTheDocument();
  });
});

describe("History: a version (UI-06, CP-02)", () => {
  it("shows its facts, where it applied, and its diff against the version before it", async () => {
    const against = mockVersion(11, { note: HOSTILE });
    renderApp("/history/11", ["viewer"]);
    expect(await screen.findByRole("heading", { name: "Version 11" })).toBeInTheDocument();
    expect(await screen.findByText(HOSTILE)).toBeInTheDocument();
    expect(screen.getByText("sub-pub")).toBeInTheDocument();
    expect(within(screen.getByRole("table", { name: "Applied on" })).getByRole("row", { name: /gateway-1/ })).toBeInTheDocument();
    const diff = await screen.findByLabelText("Diff from version 10 to version 11");
    expect(diff.querySelector(".diff-del")).toHaveTextContent("name: per-client");
    expect(screen.getByRole("list", { name: "Changes" })).toHaveTextContent("removed policy per-client");
    expect(against).toEqual([null]);
  });

  it("compares with another version, 0 the empty config, through the URL", async () => {
    const against = mockVersion(11);
    const router = renderApp("/history/11", ["viewer"]);
    await screen.findByLabelText("Diff from version 10 to version 11");
    await userEvent.type(screen.getByRole("spinbutton", { name: "Compare with version" }), "0");
    await userEvent.click(screen.getByRole("button", { name: "Compare" }));
    expect(await screen.findByLabelText("Diff from version 0 to version 11")).toBeInTheDocument();
    expect(router.state.location.search).toEqual({ against: 0 });
    expect(against).toEqual([null, "0"]);
  });

  it("says when no replica reported it", async () => {
    server.use(
      http.get("/api/v1/config/versions/4", () => HttpResponse.json({ ...version(4), snapshot: {}, applied: [] })),
      http.get("/api/v1/config/versions/4/diff", () => HttpResponse.json(diffOf(3, 4))),
    );
    renderApp("/history/4", ["viewer"]);
    expect(await screen.findByText(/No replica has reported this version/)).toBeInTheDocument();
  });

  it("shows Not found for a version that does not exist, once", async () => {
    server.use(
      http.get("/api/v1/config/versions/99", apiError(404, "not_found", "config version 99 not found")),
      http.get("/api/v1/config/versions/99/diff", apiError(404, "not_found", "config version 99 not found")),
    );
    renderApp("/history/99", ["viewer"]);
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(screen.getAllByRole("heading", { name: "Not found" })).toHaveLength(1);
  });

  it("shows Not found for a path that is not a version, without calling the API", async () => {
    renderApp("/history/abc", ["viewer"]);
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(screen.getByText("There is no config version abc.")).toBeInTheDocument();
  });

  it.each([["viewer"], ["editor"], ["key-admin"]] as const)("disables Roll back for %s with the reason", async (role) => {
    mockVersion(11);
    renderApp("/history/11", [role]);
    const b = await screen.findByRole("button", { name: "Roll back to version 11" });
    expect(b).toBeDisabled();
    expect(b).toHaveAccessibleDescription("Needs the publisher or admin role");
  });
});

describe("History: roll back (CP-04, UI-02, ADR-0060)", () => {
  async function openDialog(roles: Role[] = ["publisher"]) {
    mockVersion(10);
    renderApp("/history/10", roles);
    await userEvent.click(await screen.findByRole("button", { name: "Roll back to version 10" }));
    return screen.getByRole("dialog", { name: "Roll back to version 10?" });
  }

  it("warns about discarded edits, shows what changes against the latest, and sends the checked revision with the note", async () => {
    const validated = mockValidate(preview(7));
    const sent = mockRollback(10, () => HttpResponse.json(version(13, { kind: "rollback", rollback_of: 10, note: "back to 10" })));
    const dialog = await openDialog();
    expect(await within(dialog).findByText("Rolling back discards 2 unpublished changes in the working set.")).toBeInTheDocument();
    expect(await within(dialog).findByLabelText("Diff from version 12 to version 10")).toBeInTheDocument();
    expect(within(dialog).getByText("What changes against version 12")).toBeInTheDocument();
    expect(validated).toEqual([7]);

    // No note: nothing is sent.
    await userEvent.click(within(dialog).getByRole("button", { name: "Roll back to version 10" }));
    expect(within(dialog).getByText("A change note is required: say what the change is for.")).toBeInTheDocument();
    expect(sent).toEqual([]);

    await userEvent.type(within(dialog).getByRole("textbox", { name: "Change note (required)" }), "back to 10");
    await userEvent.click(within(dialog).getByRole("button", { name: "Roll back to version 10" }));
    expect(await screen.findByText(/Published version 13, a copy of version 10/)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("link", { name: "see it in History" })).toHaveAttribute("href", "/history/13");
    expect(sent).toEqual([{ ifMatch: '"7"', body: { note: "back to 10" } }]);
  });

  it("checks again on a 412, keeps the note, and sends the new revision only on another click", async () => {
    const validated = mockValidate(preview(7), preview(8, { changes: [], diff: "" }));
    const sent = mockRollback(10,
      apiError(412, "precondition_failed", "the working set is at revision 8"),
      () => HttpResponse.json(version(13, { kind: "rollback", rollback_of: 10, note: "n" })));
    const dialog = await openDialog(["admin"]);
    await within(dialog).findByText(/discards 2 unpublished changes/);
    await userEvent.type(within(dialog).getByRole("textbox", { name: "Change note (required)" }), "n");
    await userEvent.click(within(dialog).getByRole("button", { name: "Roll back to version 10" }));
    expect(await within(dialog).findByText(/The working set changed since this dialog opened/)).toBeInTheDocument();
    await waitFor(() => expect(validated).toEqual([7, 8]));
    await waitFor(() => expect(within(dialog).queryByText(/discards 2 unpublished changes/)).toBeNull());
    expect(within(dialog).getByRole("textbox", { name: "Change note (required)" })).toHaveValue("n");
    expect(sent).toHaveLength(1);
    await userEvent.click(within(dialog).getByRole("button", { name: "Roll back to version 10" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(sent.map((s) => s.ifMatch)).toEqual(['"7"', '"8"']);
  });

  it("lists what blocks a rollback on a 422", async () => {
    mockValidate(preview(7));
    mockRollback(10, apiError(422, "validation_failed", "the rollback is refused", [
      { entity: "consumer", id: 4, name: "acme-app", path: "plan_id", message: "refers to plan 3, which version 10 does not have" },
    ]));
    const dialog = await openDialog();
    await within(dialog).findByText(/discards 2 unpublished changes/);
    await userEvent.type(within(dialog).getByRole("textbox", { name: "Change note (required)" }), "n");
    await userEvent.click(within(dialog).getByRole("button", { name: "Roll back to version 10" }));
    expect(await within(dialog).findByText("the rollback is refused")).toBeInTheDocument();
    expect(within(dialog).getByRole("list", { name: "Validation errors" })).toHaveTextContent("consumer acme-app plan_id: refers to plan 3, which version 10 does not have");
  });

  it("shows other errors in one line with the request ID", async () => {
    mockValidate(preview(7));
    mockRollback(10, apiError(500, "internal", "boom"));
    const dialog = await openDialog();
    await within(dialog).findByText(/discards 2 unpublished changes/);
    await userEvent.type(within(dialog).getByRole("textbox", { name: "Change note (required)" }), "n");
    await userEvent.click(within(dialog).getByRole("button", { name: "Roll back to version 10" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("Something went wrong: boom (request ID req-1)");
  });

  it("says when the version is the latest, and closes on Cancel without sending", async () => {
    mockValidate(preview(7, { base_version: 10, changes: [], diff: "" }));
    const sent = mockRollback(10);
    const dialog = await openDialog();
    expect(await within(dialog).findByText("Version 10 is the latest: rolling back to it changes no published config.")).toBeInTheDocument();
    expect(within(dialog).queryByText(/discards/)).toBeNull();
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(sent).toEqual([]);
  });
});

describe("discardWarning", () => {
  it("counts the unpublished changes, or says edits are discarded when only the diff shows them", () => {
    expect(discardWarning({ diff: "", changes: [] })).toBeUndefined();
    expect(discardWarning({ diff: "x", changes: [{}] })).toBe("Rolling back discards 1 unpublished change in the working set.");
    expect(discardWarning({ diff: "x", changes: [] })).toBe("Rolling back discards the working set's unpublished edits.");
  });
});
