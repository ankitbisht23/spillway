// A unified diff as text (UI-02, ADR-0060): each line in its own element,
// classed by its first character. The diff is the server's text, never
// HTML: names and notes in it render as typed.

/** The class of a unified diff line. */
export function lineKind(line: string): "add" | "del" | "hunk" | "file" | "ctx" {
  if (line.startsWith("+++") || line.startsWith("---")) return "file";
  if (line.startsWith("@@")) return "hunk";
  if (line.startsWith("+")) return "add";
  if (line.startsWith("-")) return "del";
  return "ctx";
}

export function DiffView({ diff, label = "Diff" }: { diff: string; label?: string }) {
  if (!diff) return <p className="muted">No changes.</p>;
  const lines = diff.endsWith("\n") ? diff.slice(0, -1).split("\n") : diff.split("\n");
  return (
    <pre className="diff" aria-label={label} tabIndex={0}>
      {lines.map((line, i) => (
        <span key={i} className={`diff-line diff-${lineKind(line)}`}>
          {line}
          {"\n"}
        </span>
      ))}
    </pre>
  );
}
