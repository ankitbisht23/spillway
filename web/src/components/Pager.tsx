interface Props {
  /** The page's cursor; none on the first page. */
  cursor: string | undefined;
  /** The answer's next_cursor; null on the last page. */
  nextCursor: string | null | undefined;
  /** Goes to the page at cursor (undefined: the first), as a search param. */
  onNavigate: (cursor: string | undefined) => void;
  /** Tells apart two lists on one screen (a consumer's keys and overrides). */
  list?: string;
}

/**
 * The page before each page this app's Next opened, by list and cursor. It
 * lives as long as the page does: after a reload (which signs in again) or
 * on a pasted link, Previous is off and First still works.
 */
const previous = new Map<string, string | undefined>();

const at = (list: string, cursor: string) => `${location.pathname}\n${list}\n${cursor}`;

/**
 * Keyset pagination (UI-06, D9): First, Previous and Next, no page numbers
 * or totals. Each page's cursor is a search param. Previous goes to the
 * page Next came from, never back out of the app.
 */
export function Pager({ cursor, nextCursor, onNavigate, list = "" }: Props) {
  const back = cursor === undefined ? undefined : at(list, cursor);
  return (
    <nav className="pager" aria-label="Pages">
      <button type="button" disabled={!cursor} onClick={() => onNavigate(undefined)}>First</button>
      <button type="button" disabled={back === undefined || !previous.has(back)} onClick={() => back !== undefined && onNavigate(previous.get(back))}>Previous</button>
      <button
        type="button"
        disabled={!nextCursor}
        onClick={() => {
          if (!nextCursor) return;
          previous.set(at(list, nextCursor), cursor);
          onNavigate(nextCursor);
        }}
      >
        Next
      </button>
    </nav>
  );
}
