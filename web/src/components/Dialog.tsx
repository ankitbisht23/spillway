import { useEffect, useId, useRef, type ReactNode } from "react";

interface Props {
  title: string;
  onClose: () => void;
  children: ReactNode;
}

/** A modal dialog: focus moves into it, Escape closes it. */
export function Dialog({ title, onClose, children }: Props) {
  const id = useId();
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => {
    box.current?.querySelector<HTMLElement>("input, textarea, select, button")?.focus();
  }, []);
  return (
    <div className="modal-backdrop">
      <div
        ref={box}
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby={id}
        onKeyDown={(e) => {
          if (e.key === "Escape") onClose();
        }}
      >
        <h2 id={id}>{title}</h2>
        {children}
      </div>
    </div>
  );
}
