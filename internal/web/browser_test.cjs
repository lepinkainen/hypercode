const { test } = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const vm = require("node:vm");

test("restoring a cached page reconnects its event stream and preserves its draft", () => {
  const document = new EventTarget(), window = new EventTarget();
  const root = { dataset: { chat: "chat-1" } };
  const input = { id: "message", value: "", matches: () => false };
  const controls = { dataset: { ready: "true" } };
  document.querySelector = selector => ({ "#workspace": root, "#message": input, "#turn-controls": controls })[selector];
  const streams = [];
  class EventSource {
    constructor(url) { this.url = url; this.closed = false; streams.push(this); }
    close() { this.closed = true; }
  }
  vm.runInNewContext(readFileSync(`${__dirname}/assets/app.js`, "utf8"), {
    document, window, EventSource, setInterval() {},
  });
  document.dispatchEvent(new Event("DOMContentLoaded"));
  assert.equal(streams.length, 1);
  input.value = "unfinished draft";
  const typing = new Event("input");
  Object.defineProperty(typing, "target", { value: input });
  document.dispatchEvent(typing);
  window.dispatchEvent(new Event("pagehide"));
  assert.equal(streams[0].closed, true);
  const restored = new Event("pageshow");
  Object.defineProperty(restored, "persisted", { value: true });
  window.dispatchEvent(restored);
  assert.equal(streams.length, 2, "cached page must open a new stream");
  assert.equal(streams[1].closed, false);
  assert.equal(input.value, "unfinished draft");
  window.dispatchEvent(restored);
  assert.equal(streams.length, 2, "active page must not duplicate streams");
});
