import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { Pager } from "./Pager";

describe("Pager", () => {
  it("on the first page offers only Next", async () => {
    const onNavigate = vi.fn();
    render(<Pager cursor={undefined} nextCursor="Mg" onNavigate={onNavigate} list="first" />);
    expect(screen.getByRole("button", { name: "First" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Previous" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(onNavigate).toHaveBeenCalledWith("Mg");
  });

  it("goes back to the page Next came from, never out of the app", async () => {
    const back = vi.spyOn(history, "back").mockImplementation(() => {});
    const onNavigate = vi.fn();
    // Page 1 → 2 → 3 by Next.
    const { rerender } = render(<Pager cursor={undefined} nextCursor="NTA" onNavigate={onNavigate} list="walk" />);
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    rerender(<Pager cursor="NTA" nextCursor="MTAw" onNavigate={onNavigate} list="walk" />);
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    rerender(<Pager cursor="MTAw" nextCursor={null} onNavigate={onNavigate} list="walk" />);
    expect(screen.getByRole("button", { name: "Next" })).toBeDisabled();
    onNavigate.mockClear();
    await userEvent.click(screen.getByRole("button", { name: "Previous" }));
    expect(onNavigate).toHaveBeenLastCalledWith("NTA");
    rerender(<Pager cursor="NTA" nextCursor="MTAw" onNavigate={onNavigate} list="walk" />);
    await userEvent.click(screen.getByRole("button", { name: "Previous" }));
    expect(onNavigate).toHaveBeenLastCalledWith(undefined);
    await userEvent.click(screen.getByRole("button", { name: "First" }));
    expect(onNavigate).toHaveBeenLastCalledWith(undefined);
    expect(back).not.toHaveBeenCalled();
  });

  it("on a page it did not open (a reload or a pasted link) offers First, not Previous", async () => {
    const onNavigate = vi.fn();
    render(<Pager cursor="OTk5" nextCursor="MTA0OQ" onNavigate={onNavigate} list="pasted" />);
    expect(screen.getByRole("button", { name: "Previous" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "First" }));
    expect(onNavigate).toHaveBeenCalledWith(undefined);
  });

  it("keeps two lists on one screen apart", async () => {
    const onNavigate = vi.fn();
    const { rerender } = render(<Pager cursor={undefined} nextCursor="Mg" onNavigate={onNavigate} list="keys" />);
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    rerender(<Pager cursor="Mg" nextCursor={null} onNavigate={onNavigate} list="overrides" />);
    expect(screen.getByRole("button", { name: "Previous" })).toBeDisabled();
    rerender(<Pager cursor="Mg" nextCursor={null} onNavigate={onNavigate} list="keys" />);
    expect(screen.getByRole("button", { name: "Previous" })).toBeEnabled();
  });
});
