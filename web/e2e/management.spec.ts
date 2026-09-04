import { spawn } from "node:child_process";
import type { ChildProcess } from "node:child_process";
import { readFile } from "node:fs/promises";

import { expect, test } from "@playwright/test";
import type { Page } from "@playwright/test";

import type { components } from "../src/api/schema.gen";

type AuthSession = components["schemas"]["AuthSession"];
type Dashboard = components["schemas"]["Dashboard"];
type SecurityAuditEvent = components["schemas"]["SecurityAuditEvent"];
type SecurityAuditEventList = components["schemas"]["SecurityAuditEventList"];
type Tunnel = components["schemas"]["Tunnel"];

type JSONRecord = Record<string, unknown>;

type BrowserResult = {
  status: number;
  headers: Record<string, string>;
  body: JSONRecord | undefined;
};

async function browserRequest(
  page: Page,
  path: string,
  options: { method?: string; headers?: Record<string, string>; body?: JSONRecord } = {},
): Promise<BrowserResult> {
  return page.evaluate(async ({ requestPath, requestOptions }) => {
    const response = await fetch(requestPath, {
      method: requestOptions.method,
      headers: requestOptions.headers,
      body: requestOptions.body === undefined ? undefined : JSON.stringify(requestOptions.body),
      credentials: "include",
    });
    const text = await response.text();
    return {
      status: response.status,
      headers: Object.fromEntries(response.headers.entries()),
      body: text ? JSON.parse(text) as Record<string, unknown> : undefined,
    };
  }, { requestPath: path, requestOptions: options });
}

function errorCode(result: BrowserResult): string | undefined {
  const error = result.body?.error;
  return typeof error === "object" && error !== null && "code" in error
    ? String((error as { code: unknown }).code)
    : undefined;
}

async function browserStorageIsEmpty(page: Page, forbiddenValue?: string): Promise<boolean> {
  return page.evaluate((secret) => {
    const stores = [window.localStorage, window.sessionStorage];
    return stores.every((storage) => {
      if (storage.length !== 0) return false;
      if (!secret) return true;
      for (let index = 0; index < storage.length; index += 1) {
        const key = storage.key(index);
        if (key?.includes(secret) || storage.getItem(key ?? "")?.includes(secret)) return false;
      }
      return true;
    });
  }, forbiddenValue);
}

async function expectNoStore(headers: Record<string, string>) {
  expect(headers["cache-control"]).toBe("no-store");
  expect(headers.pragma).toBe("no-cache");
}

type AgentExit = { code: number | null; signal: NodeJS.Signals | null };

async function waitForAgentExit(exited: Promise<AgentExit>, timeout: number): Promise<AgentExit> {
  let timer: NodeJS.Timeout | undefined;
  try {
    return await Promise.race([
      exited,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error("Agent did not exit before timeout")), timeout);
      }),
    ]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

async function killAgent(agent: ChildProcess, exited: Promise<AgentExit>, verifyUnexpectedExit: boolean) {
  if (agent.exitCode === null && agent.signalCode === null && !agent.kill("SIGKILL")) {
    throw new Error("Could not send SIGKILL to Agent");
  }
  let result: AgentExit;
  try {
    result = await waitForAgentExit(exited, 10_000);
  } catch (error) {
    if (agent.exitCode === null && agent.signalCode === null) {
      agent.kill("SIGKILL");
      await waitForAgentExit(exited, 2_000).catch(() => undefined);
    }
    throw error;
  }
  if (verifyUnexpectedExit && (result.code !== null || result.signal !== "SIGKILL")) {
    throw new Error(`Agent did not preserve the injected failure: code=${String(result.code)} signal=${String(result.signal)}`);
  }
}

const proxyKind = process.env.XTUNNEL_E2E_PROXY_KIND;

test("Web-only mock 验证概览页五类最近错误渲染", async ({ page }) => {
  const sensitiveRequestID = "req_dashboard_sensitive_marker";
  const occurredAt = "2026-08-30T01:02:03Z";
  const consoleErrors: string[] = [];
  const items = [
    { code: "TUNNEL_OFFLINE", message: "当前没有可用 Tunnel。", occurred_at: occurredAt, request_id: sensitiveRequestID },
    { code: "CONNECTOR_OFFLINE", message: "Connector 已断开。", occurred_at: occurredAt, request_id: null },
    { code: "ORIGIN_DOWN", message: "源站当前不可用。", occurred_at: occurredAt, request_id: null },
    { code: "NO_CAPACITY", message: "当前没有可用容量。", occurred_at: occurredAt, request_id: null },
    { code: "PROTOCOL_ERROR", message: "<img src=x onerror=alert('unsafe')>", occurred_at: occurredAt, request_id: null },
    { code: "PROTOCOL_ERROR", message: "第二条同时协议错误。", occurred_at: occurredAt, request_id: null },
  ] satisfies Dashboard["recent_errors"]["items"];
  const session = {
    admin: { id: "adm_01J00000000000000000000000", username: "dashboard-admin" },
    csrf_token: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
    expires_at: "2026-08-31T01:02:03Z",
  } satisfies AuthSession;
  let recentErrors: Dashboard["recent_errors"] = { availability: "AVAILABLE", items };
  let gatewayCertificate: Dashboard["gateway_certificate"] = {
    tls_mode: "pinned",
    expires_at: "2027-08-30T01:02:04Z",
    remaining_seconds: 31_536_000,
    level: "HEALTHY",
    recent_renewal_failed: false,
    recent_renewal_error_code: null,
  };
  const auditListRequests: URL[] = [];
  const auditExportRequests: URL[] = [];
  const auditEvent = {
    event_id: "evt_01J00000000000000000000001",
    operation_id: "op_01J00000000000000000000001",
    event: "SECURITY_OPERATION_RESULT",
    action: "GATEWAY_KEY_ROTATE",
    actor_type: "LOCAL_OPERATOR",
    actor_id: null,
    source_ip: null,
    resource_type: "GATEWAY_IDENTITY",
    resource_id: "<img src=x onerror=alert('audit-unsafe')>",
    result: "SUCCEEDED",
    error_code: null,
    request_id: null,
    trace_id: null,
    before_state_digest: null,
    after_state_digest: null,
    occurred_at: occurredAt,
  } satisfies SecurityAuditEvent;

  page.on("console", (message) => {
    if (message.type() === "error") consoleErrors.push(message.text());
  });
  await page.route("**/api/v1/auth/me", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify(session),
    });
  });
  await page.route("**/api/v1/dashboard", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({
        server_status: "READY",
        counts: {
          tunnels_total: 0,
          tunnels_online: 0,
          tunnels_offline: 0,
          connectors_online: 0,
          services_total: 0,
          services_ready: 0,
          services_error: 0,
          active_connections: 0,
        },
        traffic: {
          availability: "AVAILABLE",
          connections_today: 0,
          ingress_bytes_today: 0,
          egress_bytes_today: 0,
        },
        recent_errors: recentErrors,
        ...(gatewayCertificate ? { gateway_certificate: gatewayCertificate } : {}),
        generated_at: "2026-08-30T01:02:04Z",
      } satisfies Dashboard),
    });
  });
  await page.route("**/api/v1/security-audit-events/export**", async (route) => {
    auditExportRequests.push(new URL(route.request().url()));
    await route.fulfill({
      status: 200,
      contentType: "application/x-ndjson",
      headers: {
        "Cache-Control": "no-store",
        "Content-Disposition": 'attachment; filename="xtunnel-security-audit.ndjson"',
      },
      body: `${JSON.stringify(auditEvent)}\n`,
    });
  });
  await page.route(/\/api\/v1\/security-audit-events(?:\?.*)?$/, async (route) => {
    const requestURL = new URL(route.request().url());
    auditListRequests.push(requestURL);
    const nextPage = requestURL.searchParams.get("page_token") === "mock-next-page";
    const pageEvent = nextPage
      ? { ...auditEvent, event_id: "evt_01J00000000000000000000002", resource_id: "mock-page-two" }
      : auditEvent;
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({
        items: [pageEvent],
        ...(!nextPage && requestURL.searchParams.get("action") === null
          ? { next_page_token: "mock-next-page" }
          : {}),
      } satisfies SecurityAuditEventList),
    });
  });

  await page.goto("/");
  await expect(page.getByText("dashboard-admin", { exact: true })).toBeVisible();
  await expect(page.locator(".certificate-panel").getByText("HEALTHY", { exact: true })).toBeVisible();
  await expect(page.locator(".error-list li")).toHaveCount(6);
  for (const label of ["Tunnel 离线", "Connector 离线", "源站异常", "容量不足"] as const) {
    await expect(page.getByText(label, { exact: true })).toBeVisible();
  }
  await expect(page.getByText("协议错误", { exact: true })).toHaveCount(2);
  await expect(page.getByText(`当前没有可用 Tunnel。 · 请求 ${sensitiveRequestID}`, { exact: true })).toBeVisible();
  await expect(page.getByText("<img src=x onerror=alert('unsafe')>", { exact: true })).toBeVisible();
  await expect(page.locator(".error-list img")).toHaveCount(0);
  expect(page.url().includes(sensitiveRequestID)).toBe(false);
  expect(await browserStorageIsEmpty(page, sensitiveRequestID)).toBe(true);
  expect(consoleErrors.some((message) => message.includes("same key"))).toBe(false);

  recentErrors = { availability: "AVAILABLE", items: [] };
  await page.getByRole("button", { name: "刷新运行状态" }).click();
  await expect(page.getByText("当前快照没有最近错误。", { exact: true })).toBeVisible();

  recentErrors = { availability: "UNAVAILABLE", items: [] };
  await page.getByRole("button", { name: "刷新运行状态" }).click();
  await expect(page.getByText("M6 Error Read Model 尚未接入。", { exact: true })).toBeVisible();

  gatewayCertificate = undefined;
  await page.getByRole("button", { name: "刷新运行状态" }).click();
  await expect(page.getByText("Gateway 证书状态不可用，请检查 Server 响应完整性。", { exact: true })).toBeVisible();

  await page.getByRole("button", { name: "安全审计" }).click();
  await expect(page.getByRole("heading", { name: "安全审计" })).toBeVisible();
  await expect(page.getByRole("cell", { name: "Gateway 身份轮换", exact: true })).toBeVisible();
  await expect(page.getByText("<img src=x onerror=alert('audit-unsafe')>", { exact: true })).toBeVisible();
  await expect(page.locator(".audit-results img")).toHaveCount(0);

  await page.getByRole("button", { name: "下一页" }).click();
  await expect(page.getByText("mock-page-two", { exact: true })).toBeVisible();
  expect(auditListRequests.at(-1)?.searchParams.get("page_token")).toBe("mock-next-page");

  await page.getByLabel("动作").selectOption("GATEWAY_KEY_ROTATE");
  await page.getByLabel("结果").selectOption("SUCCEEDED");
  await page.getByLabel("资源类型").selectOption("GATEWAY_IDENTITY");
  await page.getByLabel("资源 ID").fill(auditEvent.resource_id);
  await page.getByRole("button", { name: "应用筛选" }).click();
  await expect(page.getByText(auditEvent.resource_id, { exact: true })).toBeVisible();
  const filteredListQuery = auditListRequests.at(-1)?.searchParams;
  expect(Object.fromEntries(filteredListQuery ?? [])).toEqual({
    page_size: "50",
    action: "GATEWAY_KEY_ROTATE",
    result: "SUCCEEDED",
    resource_type: "GATEWAY_IDENTITY",
    resource_id: auditEvent.resource_id,
  });

  const downloadPromise = page.waitForEvent("download");
  await page.getByRole("button", { name: "导出 NDJSON" }).click();
  const download = await downloadPromise;
  expect(Object.fromEntries(auditExportRequests.at(-1)?.searchParams ?? [])).toEqual({
    action: "GATEWAY_KEY_ROTATE",
    result: "SUCCEEDED",
    resource_type: "GATEWAY_IDENTITY",
    resource_id: auditEvent.resource_id,
  });
  expect(download.suggestedFilename()).toBe("xtunnel-security-audit.ndjson");
  const downloadPath = await download.path();
  expect(downloadPath).toBeTruthy();
  expect((await readFile(downloadPath!, "utf8")).trim()).toContain('"event":"SECURITY_OPERATION_RESULT"');
});

test(`真实管理链路满足认证、并发与 Secret 生命周期契约（${proxyKind ?? "unknown"}）`, async ({ page, context }) => {
  const password = process.env.XTUNNEL_E2E_PASSWORD;
  const agentBinary = process.env.XTUNNEL_E2E_AGENT_BINARY;
  if (!password) throw new Error("XTUNNEL_E2E_PASSWORD is required");
  if (!agentBinary) throw new Error("XTUNNEL_E2E_AGENT_BINARY is required");
  if (proxyKind !== "caddy" && proxyKind !== "nginx") {
    throw new Error("XTUNNEL_E2E_PROXY_KIND must be caddy or nginx");
  }

  await page.goto("/");
  await expect(page.getByRole("heading", { name: "管理员登录" })).toBeVisible();

  await page.getByLabel("用户名").fill("e2e-admin");
  await page.getByLabel("密码").fill(password);
  const loginResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "POST" && new URL(response.url()).pathname === "/api/v1/auth/login",
  );
  await page.getByRole("button", { name: "进入管理控制台" }).click();
  const loginResponse = await loginResponsePromise;
  expect(loginResponse.status()).toBe(200);
  const loginHeaders = await loginResponse.allHeaders();
  await expectNoStore(loginHeaders);
  const setCookie = loginHeaders["set-cookie"] ?? "";
  expect(setCookie.includes("Secure")).toBe(true);
  expect(setCookie.includes("HttpOnly")).toBe(true);
  expect(setCookie.includes("SameSite=Lax")).toBe(true);
  expect(setCookie.includes("Path=/api/v1")).toBe(true);
  expect(setCookie.includes("Domain=")).toBe(false);

  const session = await loginResponse.json() as { csrf_token: string };
  expect(/^[A-Za-z0-9_-]{43}$/.test(session.csrf_token)).toBe(true);
  await expect(page.getByText("e2e-admin", { exact: true })).toBeVisible();

  const cookies = await context.cookies();
  const sessionCookie = cookies.find((cookie) => cookie.name === "xtunnel_admin_session");
  expect(sessionCookie?.secure).toBe(true);
  expect(sessionCookie?.httpOnly).toBe(true);
  expect(sessionCookie?.sameSite).toBe("Lax");
  expect(sessionCookie?.path).toBe("/api/v1");

  const authMe = await browserRequest(page, "/api/v1/auth/me");
  expect(authMe.status).toBe(200);
  await expectNoStore(authMe.headers);
  expect((authMe.body?.admin as JSONRecord | undefined)?.username).toBe("e2e-admin");

  const dashboardResponse = await browserRequest(page, "/api/v1/dashboard");
  expect(dashboardResponse.status).toBe(200);
  const gatewayCertificate = (dashboardResponse.body as Dashboard | undefined)?.gateway_certificate;
  expect(gatewayCertificate).toBeTruthy();
  const certificatePanel = page.locator(".certificate-panel");
  await expect(certificatePanel.getByText("UNAVAILABLE", { exact: true })).toHaveCount(0);
  await expect(certificatePanel.getByText(gatewayCertificate!.level, { exact: true })).toBeVisible();
  const renderedExpiry = await page.evaluate((expiresAt) =>
    new Date(expiresAt).toLocaleString("zh-CN", { hour12: false }), gatewayCertificate!.expires_at
  );
  await expect(certificatePanel.locator(".certificate-expiry")).toHaveText(renderedExpiry);

  const csrfRejected = await browserRequest(page, "/api/v1/tunnels", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: { name: "csrf-must-fail" },
  });
  expect(csrfRejected.status).toBe(403);
  expect(errorCode(csrfRejected)).toBe("CSRF_INVALID");

  await page.getByRole("button", { name: "服务与隧道" }).click();
  await expect(page.getByRole("heading", { name: "服务与隧道" })).toBeVisible();
  await page.getByRole("button", { name: "创建 Tunnel", exact: true }).click();
  const createTunnelDialog = page.getByRole("region", { name: "创建 Tunnel", exact: true });
  await createTunnelDialog.getByLabel("名称").fill("browser-e2e");
  const createTunnelResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "POST" && new URL(response.url()).pathname === "/api/v1/tunnels",
  );
  await createTunnelDialog.getByRole("button", { name: "创建并继续" }).click();
  const createTunnelResponse = await createTunnelResponsePromise;
  expect(createTunnelResponse.status()).toBe(201);
  await expectNoStore(await createTunnelResponse.allHeaders());
  const created = await createTunnelResponse.json() as {
    tunnel: { id: string };
    credential: { connection_token: string; deployment_commands: Array<unknown> };
  };
  const tunnelID = created.tunnel.id;
  const firstToken = created.credential.connection_token;
  expect(/^xta_[A-Za-z0-9_-]+$/.test(firstToken)).toBe(true);
  expect(created.credential.deployment_commands).toHaveLength(4);

  const credentialDialog = page.getByRole("region", { name: "安装连接器", exact: true });
  await expect(credentialDialog).toBeVisible();
  expect(await credentialDialog.locator(".credential-token code").evaluate((element, token) => element.textContent === token, firstToken)).toBe(true);
  await expect(credentialDialog.locator(".deployment-list article")).toHaveCount(1);
  await expect(credentialDialog.getByRole("button", { name: "Windows", exact: true })).toHaveAttribute("aria-pressed", "true");
  await credentialDialog.getByRole("button", { name: "前台运行", exact: true }).click();
  expect(await credentialDialog.locator(".deployment-list code").evaluate((element) => element.textContent?.startsWith(".\\xtunnel-agent.exe run --token "))).toBe(true);
  await credentialDialog.getByRole("button", { name: "Linux", exact: true }).click();
  await credentialDialog.getByRole("button", { name: "系统服务", exact: true }).click();
  expect(await credentialDialog.locator(".deployment-list code").evaluate((element) => element.textContent?.startsWith("sudo xtunnel-agent service install "))).toBe(true);
  await credentialDialog.getByRole("button", { name: "Docker", exact: true }).click();
  expect(await credentialDialog.locator(".deployment-list code").evaluate((element) => element.textContent?.startsWith("docker run "))).toBe(true);
  expect(page.url().includes(firstToken)).toBe(false);
  expect(await browserStorageIsEmpty(page, firstToken)).toBe(true);
  await credentialDialog.getByRole("button", { name: "完成", exact: true }).click();
  await expect(credentialDialog).toBeHidden();
  expect(await page.locator("body").evaluate((body, token) => !body.textContent?.includes(token), firstToken)).toBe(true);
  expect(await browserStorageIsEmpty(page, firstToken)).toBe(true);

  const tunnelPath = `/api/v1/tunnels/${tunnelID}`;
  let agentSpawnError: Error | undefined;
  // Token 只进入 Agent 子进程环境，argv 和标准流都不承载 Secret。
  const agent = spawn(agentBinary, ["run"], {
    env: { XTUNNEL_TOKEN: firstToken },
    stdio: "ignore",
  });
  agent.once("error", (error) => {
    agentSpawnError = error;
  });
  const agentExited = new Promise<AgentExit>((resolve) => {
    agent.once("exit", (code, signal) => resolve({ code, signal }));
  });
  try {
    await expect.poll(async () => {
      if (agentSpawnError) throw agentSpawnError;
      if (agent.exitCode !== null || agent.signalCode !== null) return -1;
      const result = await browserRequest(page, tunnelPath);
      if (result.status !== 200) return -1;
      return Number((result.body as Tunnel | undefined)?.connectors_online ?? -1);
    }, { timeout: 20_000 }).toBe(1);
    // SIGTERM 会先进入 DRAINING，属于预期关闭；只有强制终止才能注入真实意外断线。
    await killAgent(agent, agentExited, true);
  } finally {
    if (agent.exitCode === null && agent.signalCode === null) {
      await killAgent(agent, agentExited, false).catch(() => undefined);
    }
  }

  await expect.poll(async () => {
    const result = await browserRequest(page, "/api/v1/dashboard");
    if (result.status !== 200) return false;
    const recentErrors = result.body?.recent_errors as JSONRecord | undefined;
    const dashboardItems = Array.isArray(recentErrors?.items) ? recentErrors.items : [];
    const codes = dashboardItems.map((item) =>
      typeof item === "object" && item !== null && "code" in item ? String(item.code) : "",
    );
    return recentErrors?.availability === "AVAILABLE" &&
      codes.includes("CONNECTOR_OFFLINE") && codes.includes("TUNNEL_OFFLINE");
  }, { timeout: 20_000 }).toBe(true);

  await page.getByRole("button", { name: "概览" }).click();
  await page.getByRole("button", { name: "刷新运行状态" }).click();
  const recentErrorsPanel = page.locator(".recent-errors");
  await expect(recentErrorsPanel.getByText("AVAILABLE", { exact: true })).toBeVisible();
  const connectorOffline = recentErrorsPanel.locator("li").filter({ hasText: "Connector 离线" });
  const tunnelOffline = recentErrorsPanel.locator("li").filter({ hasText: "Tunnel 离线" });
  await expect(connectorOffline).toBeVisible();
  await expect(tunnelOffline).toBeVisible();
  await expect(connectorOffline).not.toContainText("请求");
  await expect(tunnelOffline).not.toContainText("请求");
  expect(page.url().includes(firstToken)).toBe(false);
  expect(await page.locator("body").evaluate((body, token) => !body.textContent?.includes(token), firstToken)).toBe(true);
  expect(await browserStorageIsEmpty(page, firstToken)).toBe(true);

  // 首次签发不属于敏感读取审计；先通过真实 Reveal 产生一条已提交事件，再验证
  // 查询、筛选与导出，避免用空列表冒充浏览器链路覆盖。
  const auditSeed = await browserRequest(page, `/api/v1/tunnels/${tunnelID}/token`);
  expect(auditSeed.status).toBe(200);
  await expectNoStore(auditSeed.headers);
  expect(auditSeed.body?.connection_token === firstToken).toBe(true);

  const initialAuditResponsePromise = page.waitForResponse((response) => {
    const requestURL = new URL(response.url());
    return response.request().method() === "GET" &&
      requestURL.pathname === "/api/v1/security-audit-events" &&
      requestURL.searchParams.get("page_token") === null;
  });
  await page.getByRole("button", { name: "安全审计" }).click();
  const initialAuditResponse = await initialAuditResponsePromise;
  await expect(page.getByRole("heading", { name: "安全审计" })).toBeVisible();
  await expect(page.locator(".audit-results tbody tr").first()).toBeVisible();
  const initialAudit = await initialAuditResponse.json() as SecurityAuditEventList;
  const selectedAudit = initialAudit.items[0];
  if (!selectedAudit) throw new Error("真实管理链路没有可用于筛选验证的 Security Audit Event");

  await page.getByLabel("动作").selectOption(selectedAudit.action);
  await page.getByLabel("结果").selectOption(selectedAudit.result);
  await page.getByLabel("资源类型").selectOption(selectedAudit.resource_type);
  const filteredAuditResponsePromise = page.waitForResponse((response) => {
    const requestURL = new URL(response.url());
    return response.request().method() === "GET" &&
      requestURL.pathname === "/api/v1/security-audit-events" &&
      requestURL.searchParams.get("action") === selectedAudit.action &&
      requestURL.searchParams.get("result") === selectedAudit.result &&
      requestURL.searchParams.get("resource_type") === selectedAudit.resource_type;
  });
  await page.getByRole("button", { name: "应用筛选" }).click();
  const filteredAuditResponse = await filteredAuditResponsePromise;
  const filteredAuditURL = new URL(filteredAuditResponse.url());
  expect(Object.fromEntries(filteredAuditURL.searchParams)).toEqual({
    page_size: "50",
    action: selectedAudit.action,
    result: selectedAudit.result,
    resource_type: selectedAudit.resource_type,
  });
  const filteredAudit = await filteredAuditResponse.json() as SecurityAuditEventList;
  expect(filteredAudit.items.length).toBeGreaterThan(0);
  expect(filteredAudit.items.every((event) =>
    event.action === selectedAudit.action &&
    event.result === selectedAudit.result &&
    event.resource_type === selectedAudit.resource_type
  )).toBe(true);
  await expect(page.locator(".audit-results tbody tr")).toHaveCount(filteredAudit.items.length);

  const auditDownloadPromise = page.waitForEvent("download");
  const auditResponsePromise = page.waitForResponse((response) =>
    new URL(response.url()).pathname === "/api/v1/security-audit-events/export",
  );
  await page.getByRole("button", { name: "导出 NDJSON" }).click();
  const [auditDownload, auditResponse] = await Promise.all([auditDownloadPromise, auditResponsePromise]);
  expect(auditResponse.status()).toBe(200);
  expect((await auditResponse.allHeaders())["cache-control"]).toBe("no-store");
  expect(Object.fromEntries(new URL(auditResponse.url()).searchParams)).toEqual({
    action: selectedAudit.action,
    result: selectedAudit.result,
    resource_type: selectedAudit.resource_type,
  });
  expect(auditDownload.suggestedFilename()).toBe("xtunnel-security-audit.ndjson");
  const auditDownloadPath = await auditDownload.path();
  expect(auditDownloadPath).toBeTruthy();
  const auditExport = await readFile(auditDownloadPath!, "utf8");
  const exportedAudits = auditExport.trim().split("\n").filter(Boolean).map((line) =>
    JSON.parse(line) as SecurityAuditEvent
  );
  expect(exportedAudits.length).toBeGreaterThan(0);
  expect(exportedAudits.every((event) =>
    event.action === selectedAudit.action &&
    event.result === selectedAudit.result &&
    event.resource_type === selectedAudit.resource_type
  )).toBe(true);
  expect(auditExport.includes(firstToken)).toBe(false);

  await page.getByRole("button", { name: "服务与隧道" }).click();
  await page.getByRole("button", { name: "打开隧道 browser-e2e", exact: true }).click();
  await expect(page.getByRole("heading", { name: "browser-e2e" })).toBeVisible();
  const tunnelBefore = await browserRequest(page, tunnelPath);
  expect(tunnelBefore.status).toBe(200);
  const tunnelETag = tunnelBefore.headers.etag;
  expect(Boolean(tunnelETag)).toBe(true);

  const tunnelMissingPrecondition = await browserRequest(page, tunnelPath, {
    method: "PATCH",
    headers: {
      "Content-Type": "application/merge-patch+json",
      "X-XTunnel-CSRF": session.csrf_token,
    },
    body: { name: "missing-if-match" },
  });
  expect(tunnelMissingPrecondition.status).toBe(428);
  expect(errorCode(tunnelMissingPrecondition)).toBe("PRECONDITION_REQUIRED");

  await page.getByRole("button", { name: "重命名" }).click();
  const renameTunnelDialog = page.getByRole("dialog", { name: "重命名 Tunnel" });
  await renameTunnelDialog.getByLabel("名称").fill("browser-e2e-current");
  const renameTunnelResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "PATCH" && new URL(response.url()).pathname === tunnelPath,
  );
  await renameTunnelDialog.getByRole("button", { name: "保存名称" }).click();
  expect((await renameTunnelResponsePromise).status()).toBe(200);
  await expect(page.getByRole("heading", { name: "browser-e2e-current" })).toBeVisible();

  const tunnelConflict = await browserRequest(page, tunnelPath, {
    method: "PATCH",
    headers: {
      "Content-Type": "application/merge-patch+json",
      "If-Match": tunnelETag,
      "X-XTunnel-CSRF": session.csrf_token,
    },
    body: { name: "stale-must-fail" },
  });
  expect(tunnelConflict.status).toBe(412);
  expect(errorCode(tunnelConflict)).toBe("RESOURCE_VERSION_CONFLICT");

  await page.reload();
  await expect(page.getByText("e2e-admin", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "服务与隧道" }).click();
  await page.getByRole("button", { name: "打开隧道 browser-e2e-current", exact: true }).click();
  await expect(page.getByRole("heading", { name: "browser-e2e-current" })).toBeVisible();

  const revealResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "GET" && new URL(response.url()).pathname === `/api/v1/tunnels/${tunnelID}/token`,
  );
  await page.getByRole("button", { name: "安装连接器" }).click();
  const revealResponse = await revealResponsePromise;
  expect(revealResponse.status()).toBe(200);
  await expectNoStore(await revealResponse.allHeaders());
  const revealed = await revealResponse.json() as { connection_token: string };
  expect(revealed.connection_token === firstToken).toBe(true);
  const connectorGuide = page.getByRole("dialog", { name: "安装连接器" });
  await expect(connectorGuide).toBeVisible();
  expect(await connectorGuide.locator(".credential-token code").evaluate((element, token) => element.textContent === token, firstToken)).toBe(true);
  await connectorGuide.getByRole("button", { name: "完成", exact: true }).click();
  expect(await page.locator("body").evaluate((body, token) => !body.textContent?.includes(token), firstToken)).toBe(true);
  expect(await browserStorageIsEmpty(page, firstToken)).toBe(true);

  await page.getByRole("tab", { name: /^服务/ }).click();
  await page.getByRole("button", { name: "创建 Service" }).click();
  const serviceDialog = page.getByRole("region", { name: "创建 Service" });
  await serviceDialog.getByLabel("名称").fill("browser-service");
  await serviceDialog.getByLabel("公网域名").fill("browser-service.example.test");
  const createServiceResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "POST" && new URL(response.url()).pathname === "/api/v1/services",
  );
  await serviceDialog.getByRole("button", { name: "创建 Service", exact: true }).click();
  const createServiceResponse = await createServiceResponsePromise;
  expect(createServiceResponse.status()).toBe(201);
  const service = await createServiceResponse.json() as { id: string };
  const servicePath = `/api/v1/services/${service.id}`;
  await expect(page.getByText("browser-service", { exact: true })).toBeVisible();
  await expect(page.getByText("browser-service.example.test/", { exact: true })).toBeVisible();

  const serviceMissingPrecondition = await browserRequest(page, servicePath, {
    method: "PATCH",
    headers: {
      "Content-Type": "application/merge-patch+json",
      "X-XTunnel-CSRF": session.csrf_token,
    },
    body: { name: "missing-service-if-match" },
  });
  expect(serviceMissingPrecondition.status).toBe(428);
  expect(errorCode(serviceMissingPrecondition)).toBe("PRECONDITION_REQUIRED");

  await page.getByRole("button", { name: "编辑 browser-service" }).click();
  const staleServiceDialog = page.getByRole("region", { name: "编辑 Service" });
  await expect(staleServiceDialog).toBeVisible();

  const concurrentPage = await context.newPage();
  await concurrentPage.goto("/");
  await expect(concurrentPage.getByText("e2e-admin", { exact: true })).toBeVisible();
  await concurrentPage.getByRole("button", { name: "服务与隧道" }).click();
  await concurrentPage.getByRole("button", { name: "打开隧道 browser-e2e-current", exact: true }).click();
  await expect(concurrentPage.getByRole("heading", { name: "browser-e2e-current" })).toBeVisible();
  await concurrentPage.getByRole("tab", { name: /^服务/ }).click();
  await concurrentPage.getByRole("button", { name: "编辑 browser-service" }).click();
  const concurrentEditDialog = concurrentPage.getByRole("region", { name: "编辑 Service" });
  await concurrentEditDialog.getByLabel("名称").fill("server-side-version");
  const concurrentEditResponsePromise = concurrentPage.waitForResponse((response) =>
    response.request().method() === "PATCH" && new URL(response.url()).pathname === servicePath,
  );
  await concurrentEditDialog.getByRole("button", { name: "保存 Service" }).click();
  expect((await concurrentEditResponsePromise).status()).toBe(200);
  await concurrentPage.close();

  await staleServiceDialog.getByLabel("名称").fill("stale-service-must-fail");
  const staleServiceResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "PATCH" && new URL(response.url()).pathname === servicePath,
  );
  await staleServiceDialog.getByRole("button", { name: "保存 Service" }).click();
  const staleServiceResponse = await staleServiceResponsePromise;
  expect(staleServiceResponse.status()).toBe(412);
  const staleServiceBody = await staleServiceResponse.json() as { error: { code: string } };
  expect(staleServiceBody.error.code).toBe("RESOURCE_VERSION_CONFLICT");
  await expect(staleServiceDialog).toBeHidden();
  await expect(page.getByRole("alert")).toContainText("已被其他操作更新");
  await expect(page.getByRole("alert")).toContainText("请重新打开 Service 表单");
  await expect(page.getByText("server-side-version", { exact: true })).toBeVisible();

  await page.getByRole("button", { name: "编辑 server-side-version" }).click();
  const editServiceDialog = page.getByRole("region", { name: "编辑 Service" });
  await editServiceDialog.getByLabel("名称").fill("browser-service-edited");
  const editServiceResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "PATCH" && new URL(response.url()).pathname === servicePath,
  );
  await editServiceDialog.getByRole("button", { name: "保存 Service" }).click();
  expect((await editServiceResponsePromise).status()).toBe(200);
  await expect(page.getByText("browser-service-edited", { exact: true })).toBeVisible();

  await page.getByRole("button", { name: "禁用 browser-service-edited" }).click();
  const disableDialog = page.getByRole("dialog", { name: "禁用 Service" });
  const disableResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "POST" && new URL(response.url()).pathname === `${servicePath}/disable`,
  );
  await disableDialog.getByRole("button", { name: "确认禁用" }).click();
  expect((await disableResponsePromise).status()).toBe(200);
  await expect(page.getByRole("button", { name: "启用 browser-service-edited" })).toBeVisible();

  await page.getByRole("button", { name: "启用 browser-service-edited" }).click();
  const enableDialog = page.getByRole("dialog", { name: "启用 Service" });
  const enableResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "POST" && new URL(response.url()).pathname === `${servicePath}/enable`,
  );
  await enableDialog.getByRole("button", { name: "确认启用" }).click();
  expect((await enableResponsePromise).status()).toBe(200);
  await expect(page.getByRole("button", { name: "禁用 browser-service-edited" })).toBeVisible();

  const rotateResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "POST" && new URL(response.url()).pathname === `/api/v1/tunnels/${tunnelID}/token/rotate`,
  );
  await page.getByRole("button", { name: "轮换 Token" }).click();
  await page.getByRole("dialog", { name: "轮换 Connection Token" }).getByRole("button", { name: "轮换并显示新 Token" }).click();
  const rotateResponse = await rotateResponsePromise;
  expect(rotateResponse.status()).toBe(200);
  await expectNoStore(await rotateResponse.allHeaders());
  const rotated = await rotateResponse.json() as { connection_token: string };
  expect(/^xta_[A-Za-z0-9_-]+$/.test(rotated.connection_token)).toBe(true);
  expect(rotated.connection_token === firstToken).toBe(false);
  const rotatedDialog = page.getByRole("dialog", { name: "安装连接器 · Token 已轮换" });
  await expect(rotatedDialog).toBeVisible();
  await rotatedDialog.getByRole("button", { name: "完成", exact: true }).click();
  expect(await page.locator("body").evaluate((body, token) => !body.textContent?.includes(token), rotated.connection_token)).toBe(true);
  expect(await browserStorageIsEmpty(page, rotated.connection_token)).toBe(true);

  await page.getByRole("button", { name: "删除 browser-service-edited" }).click();
  const deleteServiceDialog = page.getByRole("dialog", { name: "删除 Service" });
  const deleteServiceResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "DELETE" && new URL(response.url()).pathname === servicePath,
  );
  await deleteServiceDialog.getByRole("button", { name: "删除 Service" }).click();
  expect((await deleteServiceResponsePromise).status()).toBe(204);
  await expect(page.getByText("browser-service-edited", { exact: true })).toHaveCount(0);

  await page.getByRole("button", { name: "删除 Tunnel" }).click();
  const deleteTunnelDialog = page.getByRole("dialog", { name: "删除 Tunnel" });
  const deleteTunnelResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "DELETE" && new URL(response.url()).pathname === tunnelPath,
  );
  await deleteTunnelDialog.getByRole("button", { name: "永久删除" }).click();
  expect((await deleteTunnelResponsePromise).status()).toBe(204);
  await expect(page.getByText("还没有 Tunnel", { exact: true })).toBeVisible();

  const logoutResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "POST" && new URL(response.url()).pathname === "/api/v1/auth/logout",
  );
  await page.getByRole("button", { name: "退出" }).click();
  const logoutResponse = await logoutResponsePromise;
  expect(logoutResponse.status()).toBe(204);
  const logoutCookie = (await logoutResponse.allHeaders())["set-cookie"] ?? "";
  expect(logoutCookie.includes("Max-Age=0")).toBe(true);
  expect(logoutCookie.includes("Secure")).toBe(true);
  expect(logoutCookie.includes("HttpOnly")).toBe(true);
  await expect(page.getByRole("heading", { name: "管理员登录" })).toBeVisible();
  expect((await context.cookies()).some((cookie) => cookie.name === "xtunnel_admin_session")).toBe(false);
  expect((await browserRequest(page, "/api/v1/auth/me")).status).toBe(401);
});

test("Web-only mock 隧道详情导航、连接器分页搜索、服务入口及安装抽屉", async ({ page }) => {
  const token = "xta_mock_connector_installation";
  const tunnel = {
    id: "tun_01J00000000000000000000000", name: "安装测试", version: 1, desired_revision: 1,
    status: "PENDING", connectors_online: 0, services_count: 0, active_connections: 0,
    last_seen_at: null, first_authenticated_at: null, revoked_at: null,
    created_at: "2026-09-04T00:00:00Z", updated_at: "2026-09-04T00:00:00Z",
  } satisfies Tunnel;
  const credential = {
    tunnel_id: tunnel.id, token_id: "tok_01J00000000000000000000000", token_version: 1, status: "ACTIVE",
    connection_token: token,
    deployment_commands: [
      { environment: "FOREGROUND", command: `xtunnel-agent run --token '${token}'` },
      { environment: "CONTAINER", command: `docker run --rm -e XTUNNEL_TOKEN='${token}' xtunnel-agent:v0.1.0` },
      { environment: "LINUX_SYSTEMD", command: `sudo xtunnel-agent service install --token '${token}'` },
      { environment: "WINDOWS_SCM", command: `.\\xtunnel-agent.exe service install --token '${token}'` },
    ],
  } satisfies components["schemas"]["ConnectionCredential"];
  let revealCount = 0;
  let createTunnelRequests = 0;
  let installState: "waiting" | "connected" | "error" | "disconnected" | "unauthorized" = "waiting";
  let installPollRequests = 0;
  let installationActive = true;
  let created = false;
  let workspaceUnavailable = false;
  let createServiceRequests = 0;
  const serviceBodies: JSONRecord[] = [];
  const connectorPageRequests: URL[] = [];
  const connectors = [
    { id: "con_01J00000000000000000000001", hostname: "office-windows", os: "windows", arch: "amd64" },
    { id: "con_01J00000000000000000000002", hostname: "remote-linux", os: "linux", arch: "arm64" },
  ].map((connector) => ({
    ...connector, tunnel_id: tunnel.id, version: "v0.1.0", status: "ONLINE" as const,
    idle_work_connections: 2, active_connections: 0, connected_at: "2026-09-04T00:00:00Z",
    last_heartbeat_at: "2026-09-04T00:00:00Z", config_ready: true, observed_revision: 1,
  })) satisfies components["schemas"]["Connector"][];
  const service = {
    id: "svc_01J00000000000000000000000", tunnel_id: tunnel.id, name: "详情服务", required_revision: 1,
    origin: { scheme: "http", host: "127.0.0.1", port: 8081, connect_timeout_ms: 5000 },
    proxy_options: { disable_chunked_encoding: false, disable_happy_eyeballs: false, http_idle_connection_timeout_ms: 90000, http_max_idle_connections: 100, tcp_keepalive_interval_ms: 30000 },
    health: null, exposure: { type: "http", hostname: "detail.example.test", path_prefix: "/", preserve_host: true },
    enabled: true, version: 1, status: "READY", apply_failure: null, healthy_connectors: 1, active_connections: 0,
    usage: { availability: "AVAILABLE", connections_today: 0, ingress_bytes_today: 0, egress_bytes_today: 0 },
    created_at: "2026-09-04T00:00:00Z", updated_at: "2026-09-04T00:00:00Z",
  } satisfies components["schemas"]["Service"];
  let services: components["schemas"]["Service"][] = [];
  const unexpectedRequests: string[] = [];
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: {
      writeText: async (value: string) => { (window as unknown as { copiedCommand: string }).copiedCommand = value; },
    } });
  });
  await page.route("**/api/v1/**", async (route) => {
    const requestURL = new URL(route.request().url());
    const path = requestURL.pathname;
    let body: unknown;
    let status = 200;
    if (path === "/api/v1/auth/me") body = { admin: { id: "adm_mock", username: "mock-admin" }, csrf_token: "mock-csrf", expires_at: "2099-01-01T00:00:00Z" };
    else if (path === "/api/v1/dashboard") body = {
      server_status: "READY", counts: { tunnels_total: 0, tunnels_online: 0, tunnels_offline: 0, connectors_online: 0, services_total: 0, services_ready: 0, services_error: 0, active_connections: 0 },
      traffic: { availability: "AVAILABLE", connections_today: 0, ingress_bytes_today: 0, egress_bytes_today: 0 }, recent_errors: { availability: "AVAILABLE", items: [] }, generated_at: "2026-09-04T00:00:00Z",
    };
    else if (path === "/api/v1/tunnels" && route.request().method() === "POST") { createTunnelRequests += 1; created = true; status = 201; body = { tunnel, credential }; }
    else if (path === "/api/v1/tunnels") body = { items: created ? [tunnel] : [] };
    else if (path === `/api/v1/tunnels/${tunnel.id}`) {
      status = workspaceUnavailable ? 503 : 200;
      body = workspaceUnavailable ? { error: { code: "INTERNAL_ERROR", message: "详情暂时不可用，请重试。" } } : tunnel;
    }
    else if (path === `/api/v1/tunnels/${tunnel.id}/token`) { revealCount += 1; body = credential; }
    else if (path === `/api/v1/tunnels/${tunnel.id}/connectors`) {
      connectorPageRequests.push(requestURL);
      if (installationActive) {
        installPollRequests += 1;
        if (installState === "unauthorized") {
          status = 401;
          body = { error: { code: "AUTH_REQUIRED", message: "管理会话已过期，请重新登录。" } };
        } else if (installState === "error") {
          status = 503;
          body = { error: { code: "INTERNAL_ERROR", message: "连接器状态暂时不可用。" } };
        } else if (installState === "waiting" || installState === "disconnected") body = { items: [] };
        else body = requestURL.searchParams.get("page_token") === "connector-page-two"
          ? { items: [connectors[1]] }
          : { items: [connectors[0]], next_page_token: "connector-page-two" };
      } else body = requestURL.searchParams.get("page_token") === "connector-page-two"
        ? { items: [connectors[1]] }
        : { items: [connectors[0]], next_page_token: "connector-page-two" };
    }
    else if (path === "/api/v1/services" && route.request().method() === "POST") {
      createServiceRequests += 1;
      serviceBodies.push(route.request().postDataJSON() as JSONRecord);
      status = 400;
      body = { error: { code: "VALIDATION_FAILED", message: "公网域名不允许使用，请修改。" } };
    }
    else if (path === "/api/v1/services") body = { items: services };
    else if (path === `/api/v1/services/${service.id}`) body = services[0];
    else { unexpectedRequests.push(path); return route.abort(); }
    await route.fulfill({ status, contentType: "application/json", headers: { ETag: '"1"', "Cache-Control": "no-store" }, body: JSON.stringify(body) });
  });
  await page.goto("/");
  await page.getByRole("button", { name: "服务与隧道" }).click();
  await page.getByRole("button", { name: "创建 Tunnel", exact: true }).click();
  await page.getByLabel("名称").fill(tunnel.name);
  await page.getByRole("button", { name: "创建并继续" }).click();
  const installation = page.getByRole("region", { name: "安装连接器", exact: true });
  await expect(installation).toBeVisible();
  await expect(installation.getByText(/Token 已加密保存在服务端/)).toBeVisible();
  await expect(installation.locator(".deployment-list code")).toHaveText(credential.deployment_commands[3].command);
  await installation.getByRole("button", { name: "前台运行", exact: true }).click();
  await expect(installation.locator(".deployment-list code")).toHaveText(`.\\xtunnel-agent.exe run --token '${token}'`);
  await installation.getByRole("button", { name: "Linux", exact: true }).click();
  await expect(installation.locator(".deployment-list code")).toHaveText(credential.deployment_commands[0].command);
  await installation.getByRole("button", { name: "系统服务", exact: true }).click();
  await expect(installation.locator(".deployment-list code")).toHaveText(credential.deployment_commands[2].command);
  await installation.getByRole("button", { name: "Docker", exact: true }).click();
  await expect(installation.getByRole("group", { name: "运行方式" })).toHaveCount(0);
  await installation.getByRole("button", { name: "复制命令", exact: true }).click();
  expect(await page.evaluate(() => (window as unknown as { copiedCommand: string }).copiedCommand)).toBe(credential.deployment_commands[1].command);
  const installTable = installation.getByRole("table", { name: "安装进度连接器" });
  await expect(installation.getByText("每 3 秒自动刷新", { exact: false })).toBeVisible();
  await expect(installTable.getByText("office-windows")).toHaveCount(0);
  installState = "connected";
  await expect(installTable.getByText("office-windows", { exact: true })).toBeVisible();
  await expect(installTable.getByText("remote-linux", { exact: true })).toBeVisible();
  installState = "error";
  await expect(installation.getByRole("alert")).toContainText("已显示的列表保留供参考");
  await expect(installTable.getByText("office-windows", { exact: true })).toBeVisible();
  installState = "disconnected";
  await expect(installTable.getByText("office-windows", { exact: true })).toHaveCount(0);
  await expect(installTable.getByText("remote-linux", { exact: true })).toHaveCount(0);
  expect(createTunnelRequests).toBe(1);
  expect(revealCount).toBe(0);
  installationActive = false;
  await installation.getByRole("button", { name: "下一步：添加服务", exact: true }).click();
  const initialService = page.getByRole("region", { name: "创建 Service", exact: true });
  await expect(initialService).toBeVisible();
  await expect(initialService.getByRole("heading", { name: "公网入口", exact: true })).toBeVisible();
  await expect(initialService.getByRole("heading", { name: "源站服务", exact: true })).toBeVisible();
  await initialService.getByRole("button", { name: "取消", exact: true }).click();
  await expect(installation).toBeHidden();
  expect(await page.locator("body").evaluate((body, value) => !body.textContent?.includes(value), token)).toBe(true);
  expect(await browserStorageIsEmpty(page, token)).toBe(true);
  await expect(page.getByRole("heading", { name: tunnel.name, exact: true })).toBeVisible();
  const requestsAfterExit = connectorPageRequests.length;
  await page.waitForTimeout(3500);
  expect(connectorPageRequests).toHaveLength(requestsAfterExit);
  const drawer = page.getByRole("dialog", { name: "安装连接器", exact: true });
  await page.reload();
  await page.getByRole("button", { name: "服务与隧道" }).click();
  await page.getByRole("button", { name: `打开隧道 ${tunnel.name}`, exact: true }).click();
  for (let index = 0; index < 2; index += 1) {
    const open = page.getByRole("button", { name: "安装连接器", exact: true });
    await open.click();
    await expect(drawer.locator(".credential-token code")).toHaveText(token);
    await page.keyboard.press("Escape");
    await expect(drawer).toBeHidden();
    await expect(open).toBeFocused();
  }
  expect(revealCount).toBe(2);

  // 搜索仅作用于已加载项；分页追加后，原页及新页必须都能被检索。
  const connectorTable = page.getByRole("table", { name: "连接器列表" });
  const connectorSearch = page.getByRole("searchbox", { name: "搜索连接器" });
  await expect(page.getByRole("tab", { name: "概览", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(connectorTable.getByText("office-windows", { exact: true })).toBeVisible();
  await expect(page.getByText("搜索当前已加载的连接器", { exact: true })).toBeVisible();
  const requestsBeforeSearch = connectorPageRequests.length;
  await connectorSearch.fill("remote-linux");
  await expect(connectorTable.getByText("office-windows", { exact: true })).toHaveCount(0);
  await expect(page.getByText("显示 0 条 · 已加载 1 条", { exact: true })).toBeVisible();
  expect(connectorPageRequests).toHaveLength(requestsBeforeSearch);
  await page.getByRole("button", { name: "加载更多 Connector", exact: true }).click();
  await expect(connectorTable.getByText("remote-linux", { exact: true })).toBeVisible();
  await expect(page.getByText("显示 1 条 · 已加载 2 条", { exact: true })).toBeVisible();
  expect(connectorPageRequests.at(-1)?.searchParams.get("page_token")).toBe("connector-page-two");
  await expect(page.getByRole("button", { name: "加载更多 Connector", exact: true })).toHaveCount(0);
  await connectorSearch.fill("");
  await expect(connectorTable.locator("tbody tr")).toHaveCount(2);
  await expect(connectorTable.getByText("office-windows", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "下一页", exact: true })).toHaveCount(0);

  await page.getByRole("tab", { name: "概览", exact: true }).focus();
  await page.keyboard.press("ArrowRight");
  await expect(page.getByRole("tab", { name: /^服务/ })).toBeFocused();
  await expect(page.getByRole("tab", { name: /^服务/ })).toHaveAttribute("aria-selected", "true");
  await expect(connectorTable).toHaveCount(0);
  await page.getByRole("button", { name: "创建 Service", exact: true }).click();
  const createService = page.getByRole("region", { name: "创建 Service", exact: true });
  await createService.getByLabel("名称", { exact: true }).fill(service.name);
  await createService.getByLabel("公网域名", { exact: true }).fill("detail.example.test");
  await createService.getByRole("group", { name: "Origin 协议" }).getByRole("button", { name: "HTTPS", exact: true }).click();
  await createService.getByLabel("端口", { exact: true }).fill("443");
  await createService.getByText("高级设置", { exact: true }).click();
  await createService.getByLabel("TLS Server Name（可选）").fill("origin.example.test");
  await createService.getByLabel("Origin Host Header（可选）").fill("app.example.test");
  await createService.getByRole("button", { name: "创建 Service", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("公网域名不允许使用，请修改。");
  expect(createServiceRequests).toBe(1);
  expect(serviceBodies[0]).toMatchObject({ tunnel_id: tunnel.id, origin: { scheme: "https", port: 443, tls_verify: true, tls_server_name: "origin.example.test", http_host_header: "app.example.test" }, exposure: { type: "http", hostname: "detail.example.test", path_prefix: "/" } });
  await createService.getByRole("group", { name: "公网入口类型" }).getByRole("button", { name: "TCP", exact: true }).click();
  await createService.getByRole("group", { name: "Origin 协议" }).getByRole("button", { name: "TCP", exact: true }).click();
  await createService.getByLabel("端口", { exact: true }).fill("22");
  await createService.getByLabel("公网端口（留空自动分配）").fill("10000");
  await createService.getByRole("button", { name: "创建 Service", exact: true }).click();
  await expect.poll(() => createServiceRequests).toBe(2);
  expect(serviceBodies[1]).toMatchObject({ origin: { scheme: "tcp", port: 22 }, exposure: { type: "tcp", public_port: 10000 } });
  expect(serviceBodies[1].origin).not.toHaveProperty("tls_server_name");
  expect(serviceBodies[1].origin).not.toHaveProperty("http_host_header");
  await expect(createService.getByLabel("名称", { exact: true })).toHaveValue(service.name);
  await createService.getByRole("button", { name: "取消", exact: true }).click();
  await page.getByRole("button", { name: "关闭提示", exact: true }).click();
  services = [service];
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.getByRole("tab", { name: /^服务/ })).toHaveAttribute("aria-selected", "true");
  await page.getByRole("button", { name: `编辑 ${service.name}`, exact: true }).click();
  const editService = page.getByRole("region", { name: "编辑 Service", exact: true });
  await expect(editService.getByLabel("名称", { exact: true })).toHaveValue(service.name);
  await editService.getByRole("button", { name: "取消", exact: true }).click();
  for (const action of ["禁用", "删除"] as const) {
    await page.getByRole("button", { name: `${action} ${service.name}`, exact: true }).click();
    await expect(page.getByRole("dialog", { name: `${action} Service`, exact: true })).toBeVisible();
    await page.keyboard.press("Escape");
  }
  services = [{ ...service, enabled: false, status: "DISABLED" }];
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await page.getByRole("button", { name: `启用 ${service.name}`, exact: true }).click();
  await expect(page.getByRole("dialog", { name: "启用 Service", exact: true })).toBeVisible();
  await page.keyboard.press("Escape");

  // 手机下列表、概览和服务区域允许内部表格滚动，页面自身不能横向溢出。
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("button", { name: "返回隧道列表", exact: true }).click();
  await expect(page.getByRole("table", { name: "隧道列表" })).toBeVisible();
  const tunnelSearch = page.getByRole("searchbox", { name: "搜索隧道" });
  await tunnelSearch.fill("不存在的隧道");
  await expect(page.getByRole("button", { name: `打开隧道 ${tunnel.name}`, exact: true })).toHaveCount(0);
  await tunnelSearch.fill(tunnel.name);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.getByRole("button", { name: `打开隧道 ${tunnel.name}`, exact: true }).click();
  await expect(page.getByRole("tab", { name: "概览", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("heading", { name: "基本信息", exact: true })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.getByRole("tab", { name: /^服务/ }).click();
  await expect(page.getByRole("button", { name: `编辑 ${service.name}`, exact: true })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);

  workspaceUnavailable = true;
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("详情暂时不可用，请重试。");
  await expect(page.getByRole("button", { name: "重试", exact: true })).toBeVisible();
  workspaceUnavailable = false;
  await page.getByRole("button", { name: "重试", exact: true }).click();
  await expect(page.getByRole("heading", { name: tunnel.name, exact: true })).toBeVisible();
  await page.getByRole("button", { name: "返回隧道列表", exact: true }).click();
  await expect(page.getByRole("table", { name: "隧道列表" })).toBeVisible();
  // 安装步骤会话失效时释放凭据并退出，不能继续轮询或停留在安装页。
  installationActive = true;
  installState = "unauthorized";
  await page.getByRole("button", { name: "创建 Tunnel", exact: true }).click();
  await page.getByRole("region", { name: "创建 Tunnel", exact: true }).getByLabel("名称").fill("会话失效测试");
  await page.getByRole("button", { name: "创建并继续", exact: true }).click();
  await expect(page.getByText("管理会话已过期，请重新登录。", { exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: "安装连接器", exact: true })).toHaveCount(0);
  const requestsAfterUnauthorized = installPollRequests;
  await page.waitForTimeout(3500);
  expect(installPollRequests).toBe(requestsAfterUnauthorized);
  expect(await page.locator("body").evaluate((body, value) => !body.textContent?.includes(value), token)).toBe(true);
  expect(unexpectedRequests).toEqual([]);
});
