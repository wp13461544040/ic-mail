export function normalizeAutoLoginConfig(raw) {
  if (
    !raw ||
    typeof raw.enabled !== "boolean" ||
    !["global", "cn"].includes(raw.region) ||
    typeof raw.password_configured !== "boolean" ||
    typeof raw.code_url_configured !== "boolean"
  ) {
    throw new Error("自动保活配置读取不完整，请重新读取后保存。");
  }
  return {
    enabled: raw.enabled,
    region: raw.region,
    passwordConfigured: raw.password_configured,
    codeURLConfigured: raw.code_url_configured,
    channels: (Array.isArray(raw.channels) ? raw.channels : []).map((item) => ({
      channel: ["legacy", "icloud_web"].includes(item?.channel) ? "legacy" : ["new", "apple_account"].includes(item?.channel) ? "new" : "unknown",
      status: String(item?.status || "idle"),
      lastErrorCode: /^[A-Z0-9_]{1,80}$/.test(String(item?.last_error_code || "")) ? item.last_error_code : "",
      lastAttemptAt: item?.last_attempt_at || null,
      lastSuccessAt: item?.last_success_at || null,
      nextAttemptAt: item?.next_attempt_at || null,
    })),
  };
}

export function autoLoginValidation(form, saved = {}) {
  if (form.mailboxType === "custom" || !form.autoLoginEnabled) return {};
  const errors = {};
  if (!["global", "cn"].includes(form.autoLoginRegion)) {
    errors.autoLoginRegion = "请选择 Apple 账户区域";
  }
  if (!String(form.autoLoginPassword || "").length && !saved.passwordConfigured) {
    errors.autoLoginPassword = "请填写 Apple 账户登录密码";
  }
  const codeURL = String(form.autoLoginCodeURL || "").trim();
  if (!codeURL && !saved.codeURLConfigured) {
    errors.autoLoginCodeURL = "请填写双重验证接码链接";
  } else if (codeURL) {
    try {
      const parsed = new URL(codeURL);
      if (!["http:", "https:"].includes(parsed.protocol) || !parsed.hostname) throw new Error();
    } catch {
      errors.autoLoginCodeURL = "请填写完整的 http 或 https 接码链接";
    }
  }
  return errors;
}

export function autoLoginPayload(form) {
  const enabled = form.mailboxType !== "custom" && Boolean(form.autoLoginEnabled);
  return {
    enabled,
    region: ["global", "cn"].includes(form.autoLoginRegion) ? form.autoLoginRegion : "global",
    password: enabled ? String(form.autoLoginPassword || "") : "",
    code_url: enabled ? String(form.autoLoginCodeURL || "").trim() : "",
  };
}

export function clearAutoLoginSecrets(form) {
  form.autoLoginPassword = "";
  form.autoLoginCodeURL = "";
}

export function autoLoginStatusLabel(status) {
  return {
    idle: "等待通道需要重新登录",
    disabled: "已关闭",
    ready: "等待通道需要重新登录",
    pending: "等待自动登录",
    running: "正在自动登录",
    authenticating: "正在提交登录",
    logging_in: "正在提交登录",
    waiting_code: "正在等待双重验证码",
    cooldown: "等待下次尝试",
    retry_wait: "等待下次尝试",
    authenticated: "自动登录成功",
    needs_action: "需要检查保活配置",
    succeeded: "自动登录成功",
    success: "自动登录成功",
    failed: "自动登录未完成",
    needs_attention: "需要检查保活配置",
  }[status] || "请查看通道连接状态";
}
