// The change note every publish and rollback requires (UI-02, CP-02):
// 1 to 2,000 characters, counted as the server counts them.
import { useId } from "react";

export const MAX_NOTE = 2000;

/** The note's length in characters (code points, as the server counts). */
export function noteLength(note: string): number {
  return [...note].length;
}

/** Why the note cannot be sent, if it cannot. */
export function noteError(note: string): string | undefined {
  if (!note.trim()) return "A change note is required: say what the change is for.";
  if (noteLength(note) > MAX_NOTE) return `At most ${MAX_NOTE} characters.`;
  return undefined;
}

interface Props {
  value: string;
  onChange: (v: string) => void;
  /** Shown once the user tried to send it. */
  error: string | undefined;
  disabled?: boolean;
}

export function ChangeNote({ value, onChange, error, disabled }: Props) {
  const id = useId();
  const n = noteLength(value);
  return (
    <div className="field">
      <label htmlFor={id}>Change note (required)</label>
      <textarea
        id={id}
        value={value}
        rows={3}
        disabled={disabled}
        aria-invalid={error ? true : undefined}
        aria-describedby={`${id}-count${error ? ` ${id}-error` : ""}`}
        onChange={(e) => onChange(e.target.value)}
      />
      <span id={`${id}-count`} className={n > MAX_NOTE ? "form-error" : "muted"}>{n} / {MAX_NOTE}</span>
      {error && <span id={`${id}-error`} className="form-error" role="alert">{error}</span>}
    </div>
  );
}
