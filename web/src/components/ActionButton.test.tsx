import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { ActionButton } from "./ActionButton";

describe("ActionButton", () => {
  it("is a plain button when allowed", async () => {
    const onClick = vi.fn();
    render(<ActionButton onClick={onClick}>Publish</ActionButton>);
    const b = screen.getByRole("button", { name: "Publish" });
    expect(b).toBeEnabled();
    expect(b).toHaveAttribute("type", "button");
    await userEvent.click(b);
    expect(onClick).toHaveBeenCalledOnce();
  });

  it("is disabled with its reason, visible and as its description", async () => {
    const onClick = vi.fn();
    render(<ActionButton onClick={onClick} disabledReason="Needs the publisher or admin role">Publish</ActionButton>);
    const b = screen.getByRole("button", { name: "Publish" });
    expect(b).toBeDisabled();
    expect(b).toHaveAccessibleDescription("Needs the publisher or admin role");
    expect(screen.getByText("Needs the publisher or admin role")).toBeVisible();
    await userEvent.click(b);
    expect(onClick).not.toHaveBeenCalled();
  });
});
