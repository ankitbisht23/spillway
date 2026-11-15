// A server-sent events parser (ADR-0059): the stream is read with fetch,
// not EventSource, so the token goes in a header (D5). It follows the
// WHATWG event-stream format for what the control plane sends: event and
// data fields, comments, a blank line to dispatch, any line ending. id and
// retry are ignored: the client decides reconnects.

export interface SseEvent {
  event: string;
  data: string;
}

export class SseParser {
  private buffer = "";
  private event = "";
  private data: string[] = [];
  private dataSeen = false;
  /** A \r ended the last chunk: a \n that starts the next one is its pair. */
  private pendingCR = false;

  /** Parses a decoded chunk and returns the events it completes. */
  push(chunk: string): SseEvent[] {
    if (this.pendingCR && chunk.startsWith("\n")) chunk = chunk.slice(1);
    this.pendingCR = chunk.endsWith("\r");
    this.buffer += chunk;
    const out: SseEvent[] = [];
    for (;;) {
      const m = /\r\n|\r|\n/.exec(this.buffer);
      if (!m) break;
      const line = this.buffer.slice(0, m.index);
      this.buffer = this.buffer.slice(m.index + m[0].length);
      const e = this.line(line);
      if (e) out.push(e);
    }
    return out;
  }

  private line(line: string): SseEvent | null {
    if (line === "") {
      if (!this.dataSeen) {
        this.event = "";
        return null;
      }
      const e = { event: this.event || "message", data: this.data.join("\n") };
      this.event = "";
      this.data = [];
      this.dataSeen = false;
      return e;
    }
    if (line.startsWith(":")) return null; // a comment: a keepalive
    const colon = line.indexOf(":");
    const field = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "event") this.event = value;
    else if (field === "data") {
      this.data.push(value);
      this.dataSeen = true;
    }
    return null;
  }
}
