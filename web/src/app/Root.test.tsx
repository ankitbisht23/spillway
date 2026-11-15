import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, describe, expect, it, vi } from "vitest";

import { createQueryClient } from "../api/queryClient";
import type { Boot } from "../auth/boot";
import { createSession, setSession, SIGNED_OUT_MESSAGE, type Session } from "../auth/session";
import { provider } from "../test/keycloak";
import { appRouterAt } from "../test/render";
import { server } from "../test/server";
import { Root } from "./Root";

const me = (roles: string[]) => http.get("/api/v1/me", () => HttpResponse.json({ subject: "sub-1", name: "test-viewer", roles }));

let channel: { postMessage: () => void; close: () => void; onmessage: ((e: MessageEvent) => void) | null };

function session(): Session {
  channel = { postMessage: vi.fn(), close: vi.fn(), onmessage: null };
  const s = createSession(provider, { access: "access-0", refresh: "refresh-0", idToken: "id-0", expiresIn: 300 }, {
    now: () => 0, setTimer: () => 0, clearTimer: () => {},
    refresh: () => Promise.reject(new Error("invalid_grant")),
    fetch: (r) => fetch(r), channel, doc: document, submit: vi.fn(),
  });
  setSession(s);
  return s;
}

function renderRoot(boot: Boot, path = "/overview") {
  const signIn = vi.fn();
  const queryClient = createQueryClient();
  render(<Root boot={boot} queryClient={queryClient} createRouter={() => appRouterAt(path)} signIn={signIn} />);
  return { signIn, queryClient };
}

afterEach(() => {
  setSession(null);
  document.querySelectorAll("form").forEach((f) => f.remove());
});

describe("before a session", () => {
  it("shows Signing in while it redirects", () => {
    renderRoot({ kind: "redirecting" });
    expect(screen.getByRole("status")).toHaveTextContent("Signing in…");
  });

  it("says when the sign-in service cannot be reached", () => {
    renderRoot({ kind: "unavailable" });
    expect(screen.getByRole("heading")).toHaveTextContent("Can't reach the sign-in service");
  });

  it("shows Signed out, and signs in to the overview only on a click", async () => {
    const { signIn } = renderRoot({ kind: "signed-out", provider });
    expect(screen.getByRole("heading")).toHaveTextContent("Signed out");
    expect(signIn).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(signIn).toHaveBeenCalledWith(provider, "/overview");
  });

  it("shows a failed sign-in with its reason and a button", async () => {
    const { signIn } = renderRoot({ kind: "sign-in-failed", provider, reason: "the answer does not match this sign-in" });
    expect(screen.getByRole("alert")).toHaveTextContent("Sign-in could not be completed");
    expect(screen.getByRole("alert")).toHaveTextContent("The answer does not match this sign-in.");
    await userEvent.click(screen.getByRole("button", { name: "Sign in again" }));
    expect(signIn).toHaveBeenCalledWith(provider, "/overview");
  });
});

describe("with a session", () => {
  it("renders nothing of the app until /me answers, then the user and their roles", async () => {
    let answer: (() => void) | undefined;
    server.use(http.get("/api/v1/me", () => new Promise<Response>((resolve) => {
      answer = () => resolve(HttpResponse.json({ subject: "sub-1", name: "test-editor", roles: ["editor"] }));
    })));
    renderRoot({ kind: "session", provider, session: session() });
    expect(screen.getByRole("status")).toHaveTextContent("Signing in…");
    expect(screen.queryByRole("navigation")).toBeNull();
    await vi.waitFor(() => expect(answer).toBeDefined());
    act(() => answer?.());
    expect(await screen.findByText("test-editor")).toBeInTheDocument();
    expect(screen.getByLabelText("Roles")).toHaveTextContent("editor");
  });

  it("sends /me with the session's bearer token", async () => {
    let auth: string | null = null;
    server.use(http.get("/api/v1/me", ({ request }) => {
      auth = request.headers.get("Authorization");
      return HttpResponse.json({ subject: "s", name: "n", roles: ["viewer"] });
    }));
    renderRoot({ kind: "session", provider, session: session() });
    await screen.findByRole("navigation");
    expect(auth).toBe("Bearer access-0");
  });

  it("tells a user without a Spillway role to ask an admin, and lets them sign out", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json({ error: "forbidden", code: "forbidden", request_id: "r" }, { status: 403 })));
    const s = session();
    const signOut = vi.spyOn(s, "signOut");
    renderRoot({ kind: "session", provider, session: s });
    expect(await screen.findByRole("alert")).toHaveTextContent("Your account has no Spillway role; ask an admin.");
    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));
    expect(signOut).toHaveBeenCalledOnce();
  });

  it("shows /me's other errors with Retry", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json({ error: "boom", code: "internal", request_id: "req-1" }, { status: 500 })));
    renderRoot({ kind: "session", provider, session: session() });
    expect(await screen.findByRole("alert")).toHaveTextContent("Something went wrong");
    server.use(me(["viewer"]));
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("navigation")).toBeInTheDocument();
  });

  it("shows Your session ended over the app, and signs in again to the same path", async () => {
    server.use(me(["viewer"]));
    const s = session();
    const { signIn } = renderRoot({ kind: "session", provider, session: s });
    await screen.findByRole("navigation");
    await act(() => s.refresh());
    const dialog = screen.getByRole("alertdialog", { name: "Your session ended" });
    expect(within(dialog).getByRole("button", { name: "Sign in again" })).toHaveFocus();
    await userEvent.click(within(dialog).getByRole("button", { name: "Sign in again" }));
    expect(signIn).toHaveBeenCalledWith(provider, location.pathname + location.search);
  });

  it("shows Your session ended when it ends before /me answers", async () => {
    server.use(http.get("/api/v1/me", () => new Promise<Response>(() => {})));
    const s = session();
    renderRoot({ kind: "session", provider, session: s });
    await act(() => s.refresh());
    expect(screen.getByRole("alertdialog")).toHaveTextContent("Your session ended");
  });

  it("follows a sign-out in another tab: Signed out, at /signed-out, with the query cache cleared", async () => {
    server.use(me(["admin"]));
    const s = session();
    const { queryClient } = renderRoot({ kind: "session", provider, session: s });
    await screen.findByRole("navigation");
    expect(queryClient.getQueryData(["me"])).toBeDefined();
    act(() => channel.onmessage?.(new MessageEvent("message", { data: SIGNED_OUT_MESSAGE })));
    expect(screen.getByRole("heading")).toHaveTextContent("Signed out");
    expect(location.pathname).toBe("/signed-out");
    expect(queryClient.getQueryData(["me"])).toBeUndefined();
    history.replaceState(null, "", "/");
  });

  it("signs out from the header", async () => {
    server.use(me(["viewer"]));
    const s = session();
    const signOut = vi.spyOn(s, "signOut");
    renderRoot({ kind: "session", provider, session: s });
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(signOut).toHaveBeenCalledOnce();
  });
});
