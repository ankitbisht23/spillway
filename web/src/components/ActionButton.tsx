import { useId, type ButtonHTMLAttributes } from "react";

interface Props extends ButtonHTMLAttributes<HTMLButtonElement> {
  /** Why the action is not available; set, the button is disabled and says so. */
  disabledReason?: string;
}

/**
 * A button that, when the caller may not use it, stays visible but disabled
 * with its reason as visible text and as its accessible description (UI-04).
 */
export function ActionButton({ disabledReason, disabled, type = "button", ...props }: Props) {
  const id = useId();
  if (!disabledReason) return <button type={type} disabled={disabled} {...props} />;
  return (
    <span className="action">
      <button type={type} {...props} disabled aria-describedby={id} />
      <span id={id} className="reason">{disabledReason}</span>
    </span>
  );
}
