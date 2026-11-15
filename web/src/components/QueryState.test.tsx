import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { ApiError } from "../api/errors";
import { renderWithRouter } from "../test/render";
import { QueryState } from "./QueryState";

type Q = Parameters<typeof QueryState<string[]>>[0]["query"];
const q = (over: Partial<Q>): Q => ({ status: "success", data: undefined, error: null, refetch: vi.fn(), ...over }) as Q;
const apiError = (status: number) => new ApiError({ status, code: "internal", message: "server says", requestId: "req-7" });

describe("QueryState", () => {
  it("shows the skeleton only after 200 ms", () => {
    vi.useFakeTimers();
    render(<QueryState query={q({ status: "pending" })}>{() => "content"}</QueryState>);
    const status = screen.getByRole("status");
    expect(status).toBeEmptyDOMElement();
    act(() => vi.advanceTimersByTime(199));
    expect(status).toBeEmptyDOMElement();
    act(() => vi.advanceTimersByTime(1));
    expect(status).not.toBeEmptyDOMElement();
    expect(screen.queryByText("content")).toBeNull();
  });

  it("shows empty, or the content", () => {
    const isEmpty = (d: string[]) => d.length === 0;
    const { rerender } = render(<QueryState query={q({ data: [] })} isEmpty={isEmpty} empty="No routes yet.">{(d) => d.join()}</QueryState>);
    expect(screen.getByText("No routes yet.")).toBeInTheDocument();
    rerender(<QueryState query={q({ data: ["a", "b"] })} isEmpty={isEmpty}>{(d) => d.join()}</QueryState>);
    expect(screen.getByText("a,b")).toBeInTheDocument();
    rerender(<QueryState query={q({ data: [] })} isEmpty={isEmpty}>{(d) => d.join()}</QueryState>);
    expect(screen.getByText("Nothing here yet.")).toBeInTheDocument();
  });

  it("shows a retryable error with its request ID and retries", async () => {
    const refetch = vi.fn();
    render(<QueryState query={q({ status: "error", error: apiError(503), refetch })}>{() => "content"}</QueryState>);
    expect(screen.getByRole("alert")).toHaveTextContent("Can't reach the control plane");
    expect(screen.getByText("req-7")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(refetch).toHaveBeenCalledOnce();
  });

  it("offers no retry for an error a retry cannot fix", () => {
    render(<QueryState query={q({ status: "error", error: apiError(400) })}>{() => "content"}</QueryState>);
    expect(screen.getByRole("alert")).toHaveTextContent("The request was rejected");
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("shows Not allowed for a 403, naming the roles", async () => {
    await renderWithRouter(<QueryState query={q({ status: "error", error: apiError(403) })} needs={["publisher", "admin"]}>{() => "content"}</QueryState>);
    expect(screen.getByRole("heading", { name: "Not allowed" })).toBeInTheDocument();
    expect(screen.getByText("This needs the publisher or admin role.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Go to the overview" })).toHaveAttribute("href", "/");
  });

  it("shows Not found for a 404", async () => {
    await renderWithRouter(<QueryState query={q({ status: "error", error: apiError(404) })}>{() => "content"}</QueryState>);
    expect(screen.getByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(screen.getByText("It may have been deleted.")).toBeInTheDocument();
  });
});
