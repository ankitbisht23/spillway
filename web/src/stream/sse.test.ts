// @vitest-environment node
import { describe, expect, it } from "vitest";

import { SseParser } from "./sse";

describe("SseParser", () => {
  it("parses events, defaults the name and ignores comments, id and retry", () => {
    const p = new SseParser();
    expect(p.push(": keepalive\n\nevent: result\ndata: {\"a\":1}\n\ndata: plain\nid: 7\nretry: 10\n\n")).toEqual([
      { event: "result", data: '{"a":1}' },
      { event: "message", data: "plain" },
    ]);
  });

  it("joins multi-line data and takes a field without a colon or a space", () => {
    const p = new SseParser();
    expect(p.push("event:error\ndata:one\ndata\ndata:  two\n\n")).toEqual([{ event: "error", data: "one\n\n two" }]);
  });

  it("joins chunks split anywhere, CRLF split between chunks included", () => {
    const text = "event: result\r\ndata: {\"x\":\"é\"}\r\n\r\nevent: end\rdata: {}\r\r";
    for (let i = 1; i < text.length; i++) {
      const p = new SseParser();
      const got = [...p.push(text.slice(0, i)), ...p.push(text.slice(i))];
      expect(got, `split at ${i}`).toEqual([
        { event: "result", data: '{"x":"é"}' },
        { event: "end", data: "{}" },
      ]);
    }
  });

  it("dispatches nothing for a blank line without data, and forgets its event name", () => {
    const p = new SseParser();
    expect(p.push("event: result\n\ndata: x\n\n")).toEqual([{ event: "message", data: "x" }]);
  });

  it("holds an unfinished event until its blank line", () => {
    const p = new SseParser();
    expect(p.push("event: result\ndata: 1")).toEqual([]);
    expect(p.push("\n")).toEqual([]);
    expect(p.push("\n")).toEqual([{ event: "result", data: "1" }]);
  });
});
