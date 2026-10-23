import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../public/assets/startup-recovery-v1.js", import.meta.url), "utf8");

function fixture() {
  const handlers = new Map();
  const elements = new Map(["startup-message", "startup-reload", "resource-recovery", "resource-reload", "app"].map((id) => [id, { hidden: true, addEventListener(type, callback) { this[type] = callback; } }]));
  let deadline;
  let observe;
  let reloads = 0;
  let cleared = false;
  let disconnected = false;
  const window = { addEventListener(type, callback) { handlers.set(type, callback); }, location: { reload() { reloads += 1; } } };
  const document = { readyState: "interactive", getElementById(id) { return elements.get(id); } };
  vm.runInNewContext(source, {
    window, document,
    setTimeout(callback) { deadline = callback; return 1; },
    clearTimeout() { cleared = true; },
    MutationObserver: class { constructor(callback) { observe = callback; } observe() {} disconnect() { disconnected = true; } },
  });
  return { elements, handlers, expire: () => deadline(), mounted() { elements.delete("startup-message"); observe(); }, get reloads() { return reloads; }, get cleaned() { return cleared && disconnected; } };
}

test("initial script failure exposes recovery without automatically reloading", () => {
  const f = fixture();
  f.handlers.get("error")({ target: { tagName: "SCRIPT" } });
  assert.equal(f.elements.get("startup-reload").hidden, false);
  assert.equal(f.reloads, 0);
  f.elements.get("startup-reload").click();
  assert.equal(f.reloads, 1);
});

test("a stalled startup exposes recovery and successful mount clears the watchdog", () => {
  const f = fixture();
  f.expire();
  assert.equal(f.elements.get("startup-reload").hidden, false);
  f.mounted();
  assert.equal(f.cleaned, true);
  assert.equal(f.reloads, 0);
});

test("route import failures retain the existing page and show a recovery banner", () => {
  const f = fixture();
  f.mounted();
  f.handlers.get("error")({ target: { tagName: "IMG" } });
  assert.equal(f.elements.get("resource-recovery").hidden, true);
  f.handlers.get("icloud:startup-error")();
  assert.equal(f.elements.get("resource-recovery").hidden, false);
  assert.equal(f.elements.has("app"), true);
  assert.equal(f.reloads, 0);
});
