import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { getAccountAutoLogin, createAccount, updateAccount } from "../src/api/admin.js";
import {
  autoLoginPayload,
  autoLoginStatusLabel,
  autoLoginValidation,
  clearAutoLoginSecrets,
  normalizeAutoLoginConfig,
} from "../src/utils/autoLogin.js";

function form(overrides = {}) {
  return {
    mailboxType: "icloud",
    autoLoginEnabled: false,
    autoLoginRegion: "global",
    autoLoginPassword: "",
    autoLoginCodeURL: "",
    ...overrides,
  };
}

test("automatic login is optional until enabled and requires all three configuration fields", () => {
  assert.deepEqual(autoLoginValidation(form()), {});
  const enabled = form({ autoLoginEnabled: true, autoLoginRegion: "" });
  assert.deepEqual(Object.keys(autoLoginValidation(enabled)), [
    "autoLoginRegion", "autoLoginPassword", "autoLoginCodeURL",
  ]);
  assert.deepEqual(autoLoginValidation(form({
    autoLoginEnabled: true,
    autoLoginPassword: "sample-password",
    autoLoginCodeURL: "http://codes.example.test:56133/api/free/v4/getcode?id=1",
  })), {});
});

test("existing encrypted credentials may be retained, overwritten, or disabled without re-entry", () => {
  const saved = { passwordConfigured: true, codeURLConfigured: true };
  assert.deepEqual(autoLoginValidation(form({ autoLoginEnabled: true }), saved), {});
  assert.deepEqual(autoLoginPayload(form({ autoLoginEnabled: true })), {
    enabled: true, region: "global", password: "", code_url: "",
  });
  assert.deepEqual(autoLoginValidation(form({ autoLoginEnabled: false, autoLoginRegion: "" })), {});
  assert.deepEqual(autoLoginValidation(form({ autoLoginEnabled: true }), { passwordConfigured: true }), {
    autoLoginCodeURL: "请填写双重验证接码链接",
  });
});

test("channel state labels match persisted automatic login states", () => {
  for (const [state, label] of Object.entries({
    logging_in: "正在提交登录",
    waiting_code: "正在等待双重验证码",
    authenticated: "自动登录成功",
    retry_wait: "等待下次尝试",
    needs_action: "需要检查保活配置",
  })) {
    assert.equal(autoLoginStatusLabel(state), label);
  }
});

test("Apple account password preserves exact bytes while code URL whitespace is trimmed", () => {
  const input = form({
    autoLoginEnabled: true,
    autoLoginRegion: "cn",
    autoLoginPassword: " sample password ",
    autoLoginCodeURL: " https://codes.example.test/api/Accounts/getCode?ID=1 ",
  });
  assert.deepEqual(autoLoginPayload(input), {
    enabled: true,
    region: "cn",
    password: " sample password ",
    code_url: "https://codes.example.test/api/Accounts/getCode?ID=1",
  });
  clearAutoLoginSecrets(input);
  assert.equal(input.autoLoginPassword, "");
  assert.equal(input.autoLoginCodeURL, "");
  assert.equal(input.autoLoginEnabled, true);
});

test("code URLs accept HTTP and HTTPS providers but reject incomplete and non-HTTP links", () => {
  for (const url of ["http://codes.example.test:8818/api/Accounts/getCode?ID=1", "https://codes.example.test/code?id=1"]) {
    assert.deepEqual(autoLoginValidation(form({ autoLoginEnabled: true, autoLoginPassword: "test", autoLoginCodeURL: url })), {});
  }
  for (const url of ["codes.example.test/code", "javascript:alert(1)", "file:///code", "not a URL"]) {
    assert.equal(typeof autoLoginValidation(form({ autoLoginEnabled: true, autoLoginPassword: "test", autoLoginCodeURL: url })).autoLoginCodeURL, "string");
  }
});

test("custom mailboxes close automatic Apple login and never send entered secrets", () => {
  const input = form({ mailboxType: "custom", autoLoginEnabled: true, autoLoginPassword: "not-to-send", autoLoginCodeURL: "http://not-to-send.example.test/code" });
  assert.deepEqual(autoLoginValidation(input), {});
  assert.deepEqual(autoLoginPayload(input), { enabled: false, region: "global", password: "", code_url: "" });
});

test("configuration normalization accepts only metadata and rejects incomplete data", () => {
  const output = normalizeAutoLoginConfig({
    enabled: true, region: "cn", password_configured: true, code_url_configured: true,
    password: "should-never-return", code_url: "http://should-never-return.example.test/code",
    channels: [{ channel: "new", status: "running", last_error_code: "AUTO_LOGIN_PENDING", password: "should-never-return" }],
  });
  assert.equal(output.passwordConfigured, true);
  assert.equal(output.codeURLConfigured, true);
  assert.equal(output.channels[0].channel, "new");
  assert.equal(output.channels[0].lastErrorCode, "AUTO_LOGIN_PENDING");
  assert.doesNotMatch(JSON.stringify(output), /should-never-return/);
  for (const raw of [null, {}, { enabled: false }, { enabled: true, region: "cn", password_configured: "yes", code_url_configured: true }]) {
    assert.throws(() => normalizeAutoLoginConfig(raw), /读取不完整/);
  }
  assert.equal(autoLoginStatusLabel("unrecognized"), "请查看通道连接状态");
  const channels = normalizeAutoLoginConfig({
    enabled: false, region: "global", password_configured: false, code_url_configured: false,
    channels: [{ channel: "apple_account" }, { channel: "icloud_web" }],
  }).channels;
  assert.deepEqual(channels.map((channel) => channel.channel), ["new", "legacy"]);
});

test("auto-login API requests a separate metadata endpoint and preserves abort signal", async () => {
  const previousFetch = globalThis.fetch;
  const controller = new AbortController();
  let captured;
  globalThis.fetch = async (url, options) => {
    captured = { url, options };
    return new Response(JSON.stringify({ data: { enabled: false, region: "global", password_configured: false, code_url_configured: false, channels: [] } }), { headers: { "content-type": "application/json" } });
  };
  try {
    const output = await getAccountAutoLogin("12/34", { signal: controller.signal });
    assert.equal(captured.url, "/admin/api/v1/accounts/12%2F34/auto-login");
    assert.equal(captured.options.method, "GET");
    assert.ok(captured.options.signal);
    assert.equal(output.enabled, false);
    assert.equal(captured.options.body, undefined);
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("account create and update carry auto-login configuration in the body with CSRF", async () => {
  const previousFetch = globalThis.fetch;
  const captured = [];
  globalThis.fetch = async (url, options) => {
    captured.push({ url, options });
    return new Response(JSON.stringify({ data: { id: 12 } }), { headers: { "content-type": "application/json" } });
  };
  try {
    const auto_login = autoLoginPayload(form({ autoLoginEnabled: true, autoLoginPassword: "test-password", autoLoginCodeURL: "https://codes.example.test/code" }));
    await createAccount({ auto_login }, "test-csrf");
    await updateAccount(12, { auto_login }, "test-csrf");
    for (const { url, options } of captured) {
      assert.doesNotMatch(url, /test-password|codes\.example/);
      assert.deepEqual(JSON.parse(options.body).auto_login, auto_login);
      assert.equal(options.headers.get("X-CSRF-Token"), "test-csrf");
    }
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("account form hides credentials, blocks saving unread settings, and clears secrets on exit", async () => {
  const source = await readFile(new URL("../src/views/AccountFormView.vue", import.meta.url), "utf8");
  for (const field of ["autoLoginPassword", "autoLoginCodeURL"]) {
    const input = source.match(new RegExp(`<el-input\\s+v-model="form\\.${field}"[\\s\\S]*?/>`))?.[0];
    assert.ok(input);
    assert.match(input, /type="password"/);
    assert.doesNotMatch(input, /show-password/);
    assert.match(input, /autocomplete="new-password"/);
  }
  assert.match(source, /已设置，留空保留，可覆盖/);
  assert.match(source, /与上方收件用的 App 专用密码不同/);
  assert.match(source, /:disabled="!isCustomMailbox && \(!autoLoginReady \|\| autoLoginLoading\)"/);
  assert.match(source, /if \(!isCustomMailbox\.value && \(!autoLoginReady\.value \|\| autoLoginLoading\.value\)\) return/);
  assert.match(source, /autoLoginGate\.isCurrent\(ticket, routeKey\(\)\)/);
  assert.match(source, /onBeforeUnmount\([\s\S]*?clearAutoLoginSecrets\(form\)/);
  assert.match(source, /function cancel\(\) \{\s*clearAutoLoginSecrets\(form\)/);
  assert.doesNotMatch(source, /localStorage|sessionStorage|console\./);
});
