import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { renderApp as renderAt } from "../test/render";
import { RouteError } from "./router";

const nav = ["Overview", "Throttling", "Policies", "Publish", "Routes & upstreams", "Consumers & keys", "Tenants", "Replicas", "History", "Audit"];

describe("the app shell", () => {
  it("sends / to the overview", async () => {
    const router = renderAt("/");
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/overview");
  });

  it("lists every screen in the navigation and marks the current one", async () => {
    renderAt("/replicas");
    const links = within(await screen.findByRole("navigation", { name: "Screens" })).getAllByRole("link");
    expect(links.map((l) => l.textContent)).toEqual(nav);
    expect(screen.getByRole("link", { name: "Replicas" })).toHaveAttribute("aria-current", "page");
  });

  it("navigates", async () => {
    const router = renderAt("/overview");
    await userEvent.click(await screen.findByRole("link", { name: "History" }));
    expect(await screen.findByRole("heading", { name: "History" })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/history");
  });

  it("shows Not found for an unknown path", async () => {
    renderAt("/no/such/screen");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Go to the overview" })).toBeInTheDocument();
  });
});

describe("role-aware navigation (UI-04)", () => {
  it.each([
    ["viewer", nav.filter((n) => n !== "Tenants" && n !== "Audit")],
    ["editor", nav.filter((n) => n !== "Tenants" && n !== "Audit")],
    ["publisher", nav.filter((n) => n !== "Tenants" && n !== "Audit")],
    ["key-admin", nav.filter((n) => n !== "Tenants" && n !== "Audit")],
    ["admin", nav],
  ] as const)("shows %s exactly its screens", async (role, want) => {
    renderAt("/overview", [role]);
    const links = within(await screen.findByRole("navigation", { name: "Screens" })).getAllByRole("link");
    expect(links.map((l) => l.textContent)).toEqual(want);
  });

  it.each([["/audit", "Needs the admin role."], ["/tenants", "Needs the admin role."]])("shows Not allowed at %s to a viewer", async (path, why) => {
    renderAt(path, ["viewer"]);
    expect(await screen.findByRole("heading", { name: "Not allowed" })).toBeInTheDocument();
    expect(screen.getByText(why)).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /Audit log|Tenants/ })).toBeNull();
  });

  it("opens the hidden screens for an admin", async () => {
    renderAt("/audit", ["admin"]);
    expect(await screen.findByRole("heading", { name: "Audit log" })).toBeInTheDocument();
  });

  it("shows the user and signs out from the header", async () => {
    const signOut = vi.fn();
    renderAt("/overview", ["viewer", "publisher"], signOut);
    expect(await screen.findByText("test-user")).toBeInTheDocument();
    expect(screen.getByLabelText("Roles")).toHaveTextContent("viewer, publisher");
    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));
    expect(signOut).toHaveBeenCalledOnce();
  });
});

describe("a screen's render error", () => {
  it("shows the error state with a Reload button", async () => {
    const reload = vi.fn();
    vi.stubGlobal("location", { ...location, reload });
    render(<RouteError error={new Error("boom")} reset={() => {}} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Something went wrong");
    await userEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(reload).toHaveBeenCalledOnce();
    vi.unstubAllGlobals();
  });
});
