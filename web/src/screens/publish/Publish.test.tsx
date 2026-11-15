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

type Validation = components["schemas"]["ConfigValidation"];

const HOSTILE = `<img src=x onerror="alert(1)">`;

function preview(revision: number, over: Partial<Validation> = {}): Validation {
  return {
    working_set_revision: revision, base_version: 3, valid: true, errors: [],
    changes: [{ entity: "policy", id: 9, name: "per-client", change: "changed" }],
    diff: `--- version 3\n+++ working set (revision ${revision})\n@@ -1 +1 @@\n-  mode: shadow\n+  mode: enforce\n`,
    ...over,
  };
}

/** Serves validate with each preview in turn (the last one again after). */
function mockValidate(...previews: Validation[]) {
  const calls: number[] = [];
  server.use(http.post("/api/v1/config/validate", () => {
    const p = previews[Math.min(calls.length, previews.length - 1)] as Validation;
    calls.push(p.working_set_revision);
    return HttpResponse.json(p, { headers: { ETag: `"${p.working_set_revision}"` } });
  }));
  return calls;
}

interface Sent {
  ifMatch: string | null;
  body: unknown;
}

/** Serves publish with each answer in turn, recording what was sent. */
function mockPublish(...answers: (() => Response)[]) {
  const sent: Sent[] = [];
  server.use(http.post("/api/v1/config/publish", async ({ request }) => {
    sent.push({ ifMatch: request.headers.get("If-Match"), body: await request.json() });
    const a = answers[Math.min(sent.length - 1, answers.length - 1)];
    return a ? a() : HttpResponse.json({}, { status: 500 });
  }));
  return sent;
}

const version = (v: number, note: string) => ({
  version: v, published_at: "2026-11-16T10:00:00Z", published_by: "sub-1", note, kind: "publish", rollback_of: null, working_set_revision: 7,
  schema_version: { major: 1, minor: 0 },
});
const apiError = (status: number, code: string, error: string) => () => HttpResponse.json({ error, code, request_id: "req-1" }, { status });

beforeEach(() => {
  startSession();
  server.use(
    http.get("/api/v1/replicas", () => HttpResponse.json({ items: [], next_cursor: null })),
    http.get("/api/v1/config/versions", () => HttpResponse.json({ items: [], next_cursor: null })),
  );
});
afterEach(() => setSession(null));

async function open(roles: Role[] = ["publisher"]) {
  renderApp("/publish", roles);
  await screen.findByLabelText("Diff against the published version");
}

const note = () => screen.getByRole("textbox", { name: "Change note (required)" });
const publishButton = () => screen.getByRole("button", { name: /^Publish revision/ });

describe("the publish page (UI-02, CP-02)", () => {
  it("previews the working set: revision, changes and diff", async () => {
    mockValidate(preview(7));
    await open(["viewer"]);
    expect(screen.getByText("Working-set revision 7 against version 3.")).toBeInTheDocument();
    expect(screen.getByRole("list", { name: "Changes" })).toHaveTextContent("changed policy per-client");
    expect(screen.getByRole("link", { name: "per-client" })).toHaveAttribute("href", "/policies/9");
    expect(screen.getByLabelText("Diff against the published version").textContent).toContain("+  mode: enforce");
  });

  it("renders names, diffs and notes as text, never as HTML", async () => {
    mockValidate(preview(7, { changes: [{ entity: "policy", id: 9, name: HOSTILE, change: "added" }], diff: `+  - name: ${HOSTILE}\n` }));
    mockPublish(() => HttpResponse.json(version(4, `ship ${HOSTILE}`)));
    await open();
    await userEvent.type(note(), `ship ${HOSTILE}`);
    await userEvent.click(publishButton());
    expect(await screen.findByText(`Note: ship ${HOSTILE}`)).toBeInTheDocument();
    expect(document.querySelector("main img, main script")).toBeNull();
    expect(screen.getByRole("link", { name: HOSTILE })).toBeInTheDocument();
  });

  it("requires a change note and sends nothing without one", async () => {
    mockValidate(preview(7));
    const sent = mockPublish(() => HttpResponse.json(version(4, "x")));
    await open();
    await userEvent.click(publishButton());
    expect(await screen.findByText("A change note is required: say what the change is for.")).toBeInTheDocument();
    await userEvent.type(note(), "   ");
    await userEvent.click(publishButton());
    expect(note()).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText("3 / 2000")).toBeInTheDocument();
    expect(sent).toEqual([]);
  });

  it("publishes with If-Match on the previewed revision and the note, then shows the version", async () => {
    mockValidate(preview(7), preview(8, { changes: [], diff: "", base_version: 4 }));
    const sent = mockPublish(() => HttpResponse.json(version(4, "enforce per-client")));
    await open();
    await userEvent.type(note(), "enforce per-client");
    await userEvent.click(publishButton());
    const done = await screen.findByText(/Published version 4/);
    expect(sent).toEqual([{ ifMatch: '"7"', body: { note: "enforce per-client" } }]);
    expect(within(done.closest(".banner") as HTMLElement).getByRole("link", { name: "see it in History" })).toHaveAttribute("href", "/history/4");
    expect(await screen.findByText("No live replica reports applied versions (static gateways do not).")).toBeInTheDocument();
    // The preview refreshed: nothing left to publish.
    expect(await screen.findByText("Nothing to publish: the working set is version 4")).toBeInTheDocument();
    expect(note()).toHaveValue("");
  });

  it("on 412 shows the newer diff, keeps the note and waits for another click", async () => {
    const validated = mockValidate(preview(7), preview(9, {
      changes: [{ entity: "policy", id: 9, name: "per-client", change: "changed" }, { entity: "route", id: 2, name: "orders", change: "changed" }],
      diff: "--- version 3\n+++ working set (revision 9)\n+  timeout: 5s\n",
    }));
    const sent = mockPublish(apiError(412, "precondition_failed", "If-Match does not match: the current revision is 9"), () => HttpResponse.json(version(4, "n")));
    await open();
    await userEvent.type(note(), "enforce per-client");
    await userEvent.click(publishButton());
    expect(await screen.findByText(/The working set changed since your preview/)).toBeInTheDocument();
    expect(await screen.findByText("Working-set revision 9 against version 3.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "orders" })).toBeInTheDocument();
    expect(screen.getByLabelText("Diff against the published version").textContent).toContain("+  timeout: 5s");
    expect(note()).toHaveValue("enforce per-client");
    expect(sent).toHaveLength(1);
    expect(validated).toEqual([7, 9]);
    await userEvent.click(screen.getByRole("button", { name: "Publish revision 9" }));
    await screen.findByText(/Published version 4/);
    expect(sent.map((s) => s.ifMatch)).toEqual(['"7"', '"9"']);
  });

  it("on 409 says there is nothing to publish and refreshes", async () => {
    const validated = mockValidate(preview(7), preview(7, { changes: [], diff: "", base_version: 5 }));
    mockPublish(apiError(409, "conflict", "nothing to publish: the working set has not changed since version 5"));
    await open();
    await userEvent.type(note(), "n");
    await userEvent.click(publishButton());
    expect(await screen.findByText("Nothing to publish: the working set is already version 5.")).toBeInTheDocument();
    await waitFor(() => expect(validated).toHaveLength(2));
    expect(await screen.findByText("Nothing to publish: the working set is version 5")).toBeInTheDocument();
  });

  it("shows a 422's validation errors", async () => {
    mockValidate(preview(7));
    mockPublish(() => HttpResponse.json({
      error: "validation failed: 1 error", code: "validation_failed", request_id: "r",
      details: [{ entity: "policy", id: 9, name: "per-client", message: "conflicts with per-ip" }],
    }, { status: 422 }));
    await open();
    await userEvent.type(note(), "n");
    await userEvent.click(publishButton());
    expect(await screen.findByRole("list", { name: "Validation errors" })).toHaveTextContent("policy per-client: conflicts with per-ip");
  });

  it("shows other errors with the request ID", async () => {
    mockValidate(preview(7));
    mockPublish(apiError(500, "internal", "internal error"));
    await open();
    await userEvent.type(note(), "n");
    await userEvent.click(publishButton());
    expect(await screen.findByRole("alert")).toHaveTextContent("Something went wrong: internal error (request ID req-1)");
  });

  it.each<[Role, string | null]>([
    ["viewer", "Needs the publisher or admin role"],
    ["editor", "Needs the publisher or admin role"],
    ["key-admin", "Needs the publisher or admin role"],
    ["publisher", null],
    ["admin", null],
  ])("for %s: Publish is %s", async (role, reason) => {
    mockValidate(preview(7));
    await open([role]);
    if (reason) {
      expect(publishButton()).toBeDisabled();
      expect(publishButton()).toHaveAccessibleDescription(reason);
    } else {
      expect(publishButton()).toBeEnabled();
    }
  });

  it("disables Publish while the preview has errors, linking the entity at fault", async () => {
    mockValidate(preview(7, { valid: false, errors: [{ entity: "policy", id: 9, name: "per-client", path: "limits", message: "burst required" }] }));
    await open();
    expect(publishButton()).toHaveAccessibleDescription("Fix the errors above first");
    expect(screen.getByRole("list", { name: "Validation errors" })).toHaveTextContent("policy per-client limits: burst required");
  });

  it("shows applied on n of m live replicas", async () => {
    mockValidate(preview(7));
    mockPublish(() => HttpResponse.json(version(4, "n")));
    server.use(
      http.get("/api/v1/replicas", () => HttpResponse.json({ items: [{ id: "gateway-1", live: true }, { id: "gateway-2", live: true }, { id: "gateway-3", live: false }], next_cursor: null })),
      http.get("/api/v1/config/versions/4", () => HttpResponse.json({ ...version(4, "n"), applied: [{ replica: "gateway-1", at: "x" }, { replica: "gateway-2", at: "y" }] })),
    );
    await open();
    await userEvent.type(note(), "n");
    await userEvent.click(publishButton());
    expect(await screen.findByText("Applied on 2 of 2 live replicas.")).toBeInTheDocument();
  });

  it("shows the preview's error state with a retry", async () => {
    server.use(http.post("/api/v1/config/validate", () => HttpResponse.json({ error: "x", code: "internal", request_id: "r9" }, { status: 500 })));
    renderApp("/publish", ["viewer"]);
    expect(await screen.findByRole("alert")).toHaveTextContent("Something went wrong");
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });
});
