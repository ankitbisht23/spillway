import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { Pager } from "./Pager";

describe("Pager", () => {
  it("on the first page offers only Next", async () => {
    const onNavigate = vi.fn();
    render(<Pager cursor={undefined} nextCursor="Mg" onNavigate={onNavigate} />);
    expect(screen.getByRole("button", { name: "First" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Previous" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(onNavigate).toHaveBeenCalledWith("Mg");
  });

  it("on the last page goes back or to the first page", async () => {
    const onNavigate = vi.fn();
    const back = vi.spyOn(history, "back").mockImplementation(() => {});
    render(<Pager cursor="Mg" nextCursor={null} onNavigate={onNavigate} />);
    expect(screen.getByRole("button", { name: "Next" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Previous" }));
    expect(back).toHaveBeenCalledOnce();
    await userEvent.click(screen.getByRole("button", { name: "First" }));
    expect(onNavigate).toHaveBeenCalledWith(undefined);
  });
});
