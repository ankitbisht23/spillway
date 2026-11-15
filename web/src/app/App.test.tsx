import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { createQueryClient } from "../api/queryClient";
import { appRouterAt } from "../test/render";
import { App } from "./App";
import { RouteError } from "./router";

function renderAt(path: string) {
  const router = appRouterAt(path);
  render(<App router={router} queryClient={createQueryClient()} />);
  return router;
}

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

  it("shows a placeholder naming the task for a screen not built yet", async () => {
    renderAt("/policies/12/shadow");
    expect(await screen.findByRole("heading", { name: "Shadow report" })).toBeInTheDocument();
    expect(screen.getByText("Arrives with m6-policies.")).toBeInTheDocument();
  });

  it("navigates", async () => {
    const router = renderAt("/overview");
    await userEvent.click(await screen.findByRole("link", { name: "History" }));
    expect(await screen.findByText("Arrives with m6-replicas-history.")).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/history");
  });

  it("shows Not found for an unknown path", async () => {
    renderAt("/no/such/screen");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Go to the overview" })).toBeInTheDocument();
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
