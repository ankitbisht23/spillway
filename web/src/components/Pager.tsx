interface Props {
  /** The page's cursor; none on the first page. */
  cursor: string | undefined;
  /** The answer's next_cursor; null on the last page. */
  nextCursor: string | null | undefined;
  /** Goes to the page at cursor (undefined: the first), as a search param. */
  onNavigate: (cursor: string | undefined) => void;
}

/**
 * Keyset pagination (UI-06, D9): First, Previous and Next, no page numbers
 * or totals. Each page's cursor is a search param, so Previous is the
 * browser's Back.
 */
export function Pager({ cursor, nextCursor, onNavigate }: Props) {
  return (
    <nav className="pager" aria-label="Pages">
      <button type="button" disabled={!cursor} onClick={() => onNavigate(undefined)}>First</button>
      <button type="button" disabled={!cursor} onClick={() => history.back()}>Previous</button>
      <button type="button" disabled={!nextCursor} onClick={() => nextCursor && onNavigate(nextCursor)}>Next</button>
    </nav>
  );
}
