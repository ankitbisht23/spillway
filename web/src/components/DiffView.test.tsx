import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { renderWithRouter } from "../test/render";
import { ChangesList, ValidationErrors } from "./Changes";
import { DiffView, lineKind } from "./DiffView";

const HOSTILE = `<img src=x onerror="alert(1)"><script>alert(2)</script>`;

describe("DiffView (UI-02)", () => {
  it("shows each line, classed by its kind", () => {
    const diff = "--- version 3\n+++ working set (revision 9)\n@@ -1,2 +1,2 @@\n policies:\n-  - mode: shadow\n+  - mode: enforce\n";
    const { container } = render(<DiffView diff={diff} />);
    const lines = [...container.querySelectorAll(".diff-line")];
    expect(lines.map((l) => l.className.replace("diff-line ", ""))).toEqual(["diff-file", "diff-file", "diff-hunk", "diff-ctx", "diff-del", "diff-add"]);
    expect(screen.getByLabelText("Diff").textContent).toContain("\n+  - mode: enforce\n");
    expect(lineKind("+++ b")).toBe("file");
  });

  it("says when nothing changed", () => {
    render(<DiffView diff="" />);
    expect(screen.getByText("No changes.")).toBeInTheDocument();
  });

  it("renders names and notes in a diff as text, never as HTML", () => {
    const { container } = render(<DiffView diff={`+  - name: ${HOSTILE}\n`} />);
    expect(container.querySelector("img, script")).toBeNull();
    expect(screen.getByLabelText("Diff").textContent).toBe(`+  - name: ${HOSTILE}\n`);
  });
});

describe("ChangesList and ValidationErrors", () => {
  it("link entities with a screen and render names as text", async () => {
    const { container } = await renderWithRouter(
      <>
        <ChangesList changes={[
          { entity: "policy", id: 4, name: HOSTILE, change: "added" },
          { entity: "plan", id: 2, name: "free", change: "removed" },
        ]} />
        <ValidationErrors errors={[{ entity: "route", id: 3, name: "orders", path: "upstream", message: `no upstream ${HOSTILE}` }, { message: "global problem" }]} />
      </>,
    );
    expect(container.querySelector("img, script")).toBeNull();
    expect(screen.getByRole("link", { name: HOSTILE })).toHaveAttribute("href", "/policies/4");
    expect(screen.getByRole("list", { name: "Changes" })).toHaveTextContent("removed plan free");
    expect(screen.getByRole("link", { name: "orders" })).toHaveAttribute("href", "/routes/3");
    expect(screen.getByRole("list", { name: "Validation errors" })).toHaveTextContent(`route orders upstream: no upstream ${HOSTILE}`);
    expect(screen.getByText("global problem")).toBeInTheDocument();
  });

  it("says when no entity changed", () => {
    render(<ChangesList changes={[]} />);
    expect(screen.getByText("No entity changed.")).toBeInTheDocument();
  });
});
