import assert from "node:assert/strict";
import test from "node:test";

import {
  ApiError,
  apiRequest,
  setUnauthorizedHandler,
} from "../src/api/client.js";

function errorResponse(status, code) {
  return new Response(
    JSON.stringify({
      error: {
        code,
        message: "test error",
        request_id: "request-test-id",
      },
    }),
    {
      status,
      headers: { "Content-Type": "application/json" },
    },
  );
}

test("current-password errors do not invalidate the session", async () => {
  let invalidations = 0;
  setUnauthorizedHandler(() => {
    invalidations += 1;
  });
  globalThis.fetch = async () =>
    errorResponse(401, "CURRENT_PASSWORD_INVALID");

  await assert.rejects(
    apiRequest("/auth/password", {
      method: "PUT",
      body: {},
      csrfToken: "csrf-test-token",
    }),
    (error) =>
      error instanceof ApiError &&
      error.code === "CURRENT_PASSWORD_INVALID" &&
      error.requestId === "request-test-id",
  );
  assert.equal(invalidations, 0);
});

test("expired sessions invoke the global unauthorized handler", async () => {
  let invalidations = 0;
  setUnauthorizedHandler(() => {
    invalidations += 1;
  });
  globalThis.fetch = async () => errorResponse(401, "SESSION_EXPIRED");

  await assert.rejects(apiRequest("/accounts"), {
    name: "ApiError",
    code: "SESSION_EXPIRED",
  });
  assert.equal(invalidations, 1);
  setUnauthorizedHandler(null);
});

test("HTML gateway timeouts are normalized without exposing the proxy page", async () => {
  const proxyPage =
    "<!doctype html><html><body><h1>504 Gateway Time-out</h1></body></html>";
  globalThis.fetch = async () =>
    new Response(proxyPage, {
      status: 504,
      headers: { "Content-Type": "text/html" },
    });

  await assert.rejects(
    apiRequest("/accounts/12/sync", { method: "POST" }),
    (error) => {
      assert.ok(error instanceof ApiError);
      assert.equal(error.status, 504);
      assert.equal(error.code, "GATEWAY_TIMEOUT");
      assert.equal(error.message, "网关等待服务响应超时，请稍后重试。");
      assert.equal(error.message.includes("<html>"), false);
      return true;
    },
  );
});

test("a stalled request times out and aborts without retrying a submitted write", async () => {
  let calls = 0;
  let transportSignal;
  globalThis.fetch = (_url, { signal }) => {
    calls += 1;
    transportSignal = signal;
    return new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true }));
  };
  await assert.rejects(apiRequest("/aliases/batch", { method: "POST", body: {}, timeoutMs: 20 }), { code: "REQUEST_TIMEOUT" });
  assert.equal(transportSignal.aborted, true);
  assert.equal(calls, 1);
});

test("the deadline also bounds a stalled response body", async () => {
  globalThis.fetch = async (_url, { signal }) => ({
    status: 200,
    ok: true,
    headers: new Headers({ "Content-Type": "application/json" }),
    json: () => new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true })),
  });
  await assert.rejects(apiRequest("/accounts", { timeoutMs: 20 }), { code: "REQUEST_TIMEOUT" });
});

test("caller cancellation reaches the transport and stays an AbortError", async () => {
  const caller = new AbortController();
  let transportSignal;
  globalThis.fetch = (_url, { signal }) => {
    transportSignal = signal;
    return new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true }));
  };
  const result = apiRequest("/accounts", { signal: caller.signal });
  caller.abort();
  await assert.rejects(result, { name: "AbortError" });
  assert.equal(transportSignal.aborted, true);
});

test("completed reads clear their deadline and do not later abort", async () => {
  let transportSignal;
  globalThis.fetch = async (_url, { signal }) => {
    transportSignal = signal;
    return new Response(JSON.stringify({ data: { ready: true } }), { headers: { "Content-Type": "application/json" } });
  };
  assert.deepEqual(await apiRequest("/accounts", { timeoutMs: 10 }), { ready: true });
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(transportSignal.aborted, false);
});
