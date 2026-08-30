import { spawn } from "node:child_process";
import type { ChildProcess } from "node:child_process";

import { expect, test } from "@playwright/test";
import type { Page } from "@playwright/test";

import type { components } from "../src/api/schema.gen";

type AuthSession = components["schemas"]["AuthSession"];
type Dashboard = components["schemas"]["Dashboard"];
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
        generated_at: "2026-08-30T01:02:04Z",
      } satisfies Dashboard),
    });
  });

  await page.goto("/");
  await expect(page.getByText("dashboard-admin", { exact: true })).toBeVisible();
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

  const csrfRejected = await browserRequest(page, "/api/v1/tunnels", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: { name: "csrf-must-fail" },
  });
  expect(csrfRejected.status).toBe(403);
  expect(errorCode(csrfRejected)).toBe("CSRF_INVALID");

  await page.getByRole("button", { name: "服务与隧道" }).click();
  await expect(page.getByRole("heading", { name: "链路工作台" })).toBeVisible();
  await page.getByRole("button", { name: "创建 Tunnel", exact: true }).click();
  const createTunnelDialog = page.getByRole("dialog", { name: "创建 Tunnel" });
  await createTunnelDialog.getByLabel("名称").fill("browser-e2e");
  const createTunnelResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "POST" && new URL(response.url()).pathname === "/api/v1/tunnels",
  );
  await createTunnelDialog.getByRole("button", { name: "创建并显示部署指引" }).click();
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

  const credentialDialog = page.getByRole("dialog", { name: "部署第一个 Connector" });
  await expect(credentialDialog).toBeVisible();
  expect(await credentialDialog.locator(".credential-token code").evaluate((element, token) => element.textContent === token, firstToken)).toBe(true);
  await expect(credentialDialog.locator(".deployment-list article")).toHaveCount(4);
  expect(page.url().includes(firstToken)).toBe(false);
  expect(await browserStorageIsEmpty(page, firstToken)).toBe(true);
  await credentialDialog.getByRole("button", { name: "完成并清除页面 Secret" }).click();
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

  await page.getByRole("button", { name: "服务与隧道" }).click();
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
  await expect(page.getByRole("heading", { name: "browser-e2e-current" })).toBeVisible();

  const revealResponsePromise = page.waitForResponse((response) =>
    response.request().method() === "GET" && new URL(response.url()).pathname === `/api/v1/tunnels/${tunnelID}/token`,
  );
  await page.getByRole("button", { name: "添加 Connector" }).click();
  const revealResponse = await revealResponsePromise;
  expect(revealResponse.status()).toBe(200);
  await expectNoStore(await revealResponse.allHeaders());
  const revealed = await revealResponse.json() as { connection_token: string };
  expect(revealed.connection_token === firstToken).toBe(true);
  const connectorGuide = page.getByRole("dialog", { name: "添加 Connector" });
  await expect(connectorGuide).toBeVisible();
  expect(await connectorGuide.locator(".credential-token code").evaluate((element, token) => element.textContent === token, firstToken)).toBe(true);
  await connectorGuide.getByRole("button", { name: "完成并清除页面 Secret" }).click();
  expect(await page.locator("body").evaluate((body, token) => !body.textContent?.includes(token), firstToken)).toBe(true);
  expect(await browserStorageIsEmpty(page, firstToken)).toBe(true);

  await page.getByRole("button", { name: "创建 Service" }).click();
  const serviceDialog = page.getByRole("dialog", { name: "创建 Service" });
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
  const staleServiceDialog = page.getByRole("dialog", { name: "编辑 Service" });
  await expect(staleServiceDialog).toBeVisible();

  const concurrentPage = await context.newPage();
  await concurrentPage.goto("/");
  await expect(concurrentPage.getByText("e2e-admin", { exact: true })).toBeVisible();
  await concurrentPage.getByRole("button", { name: "服务与隧道" }).click();
  await expect(concurrentPage.getByRole("heading", { name: "browser-e2e-current" })).toBeVisible();
  await concurrentPage.getByRole("button", { name: "编辑 browser-service" }).click();
  const concurrentEditDialog = concurrentPage.getByRole("dialog", { name: "编辑 Service" });
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
  const editServiceDialog = page.getByRole("dialog", { name: "编辑 Service" });
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
  const rotatedDialog = page.getByRole("dialog", { name: "Token 已轮换" });
  await expect(rotatedDialog).toBeVisible();
  await rotatedDialog.getByRole("button", { name: "完成并清除页面 Secret" }).click();
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
