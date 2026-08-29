import {
  Activity,
  AlertTriangle,
  Bot,
  Cable,
  CircleCheck,
  KeyRound,
  LayoutDashboard,
  LoaderCircle,
  LockKeyhole,
  LogOut,
  Network,
  RadioTower,
  RefreshCw,
  Server,
  Settings,
  ShieldCheck,
} from "lucide-react";
import { useEffect, useId, useState } from "react";
import type { FormEvent } from "react";

import { apiClient } from "./api/client";
import type { components } from "./api/schema.gen";
import { ManagementView } from "./management";

type AuthSession = components["schemas"]["AuthSession"];
type Dashboard = components["schemas"]["Dashboard"];
type APIErrorEnvelope =
  | components["schemas"]["ErrorResponse"]
  | components["schemas"]["SetupRequiredErrorResponse"];

type AuthState =
  | { status: "checking" }
  | { status: "anonymous"; message?: string }
  | { status: "authenticated"; session: AuthSession };

type DashboardState =
  | { status: "loading" }
  | { status: "ready"; dashboard: Dashboard }
  | { status: "error"; message: string };

type ConsolePage = "overview" | "management";

const navigation: ReadonlyArray<{
  label: string;
  icon: typeof LayoutDashboard;
  page?: ConsolePage;
}> = [
  { label: "概览", icon: LayoutDashboard, page: "overview" },
  { label: "Agent 管理", icon: Bot },
  { label: "服务与隧道", icon: Network, page: "management" },
  { label: "访问入口", icon: RadioTower },
  { label: "系统设置", icon: Settings },
] as const;

function isAbortError(error: unknown) {
  return error instanceof DOMException && error.name === "AbortError";
}

function apiErrorMessage(error: APIErrorEnvelope | undefined) {
  return error?.error.message;
}

function apiErrorCode(error: APIErrorEnvelope | undefined) {
  return error?.error.code;
}

function Brand() {
  return (
    <div className="brand" aria-label="XTunnel">
      <span className="brand-mark" aria-hidden="true">
        XT
      </span>
      <span className="brand-name">XTunnel</span>
    </div>
  );
}

function SessionCheck() {
  return (
    <main className="session-check" aria-live="polite" aria-busy="true">
      <div className="session-check-card">
        <Brand />
        <LoaderCircle className="session-check-spinner" aria-hidden="true" />
        <p>正在确认管理会话…</p>
      </div>
    </main>
  );
}

function LoginPage({ initialMessage, onLogin }: {
  initialMessage?: string;
  onLogin: (session: AuthSession) => void;
}) {
  const usernameId = useId();
  const passwordId = useId();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [message, setMessage] = useState(initialMessage);
  const [setupRequired, setSetupRequired] = useState(false);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitting(true);
    setMessage(undefined);
    setSetupRequired(false);

    try {
      const result = await apiClient.POST("/auth/login", {
        params: { header: { Origin: window.location.origin } },
        body: { username, password },
      });

      if (result.data) {
        setPassword("");
        onLogin(result.data);
        return;
      }

      const error = result.error as APIErrorEnvelope | undefined;
      switch (result.response.status) {
        case 409:
          setSetupRequired(true);
          setMessage("此 Server 尚未创建管理员，请先在 Server 本机完成初始化。");
          break;
        case 401:
          setMessage("用户名或密码不正确，请重新输入。");
          break;
        case 429: {
          const retryAfter = result.response.headers.get("Retry-After");
          setMessage(
            retryAfter
              ? `登录尝试过多，请在 ${retryAfter} 秒后重试。`
              : "登录尝试过多，请稍后重试。",
          );
          break;
        }
        default:
          setMessage(apiErrorMessage(error) ?? "登录暂时不可用，请稍后重试。");
      }
    } catch {
      setMessage("无法连接管理服务，请检查 Server 是否正在运行。");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <main className="login-shell">
      <section className="login-intro" aria-labelledby="login-intro-title">
        <div className="login-brand-row">
          <Brand />
          <span>Standalone · V0.1</span>
        </div>

        <div className="login-intro-copy">
          <p className="login-kicker">LOCAL CONTROL PLANE</p>
          <h1 id="login-intro-title">从这里进入你的隧道控制面。</h1>
          <p>
            管理 Session 仅通过同源安全 Cookie 建立。CSRF 凭据只保留在当前页面内存中，关闭页面即释放。
          </p>
        </div>

        <div className="login-signal-list" aria-label="安全边界">
          <div>
            <ShieldCheck aria-hidden="true" />
            <span><strong>同源校验</strong>Host 与 Origin 必须匹配</span>
          </div>
          <div>
            <LockKeyhole aria-hidden="true" />
            <span><strong>安全会话</strong>Secure、HttpOnly Cookie</span>
          </div>
          <div>
            <Server aria-hidden="true" />
            <span><strong>本机初始化</strong>首个管理员不经公网创建</span>
          </div>
        </div>
      </section>

      <section className="login-panel" aria-labelledby="login-title">
        <div className="login-card">
          <div className="login-card-heading">
            <span className="login-card-icon" aria-hidden="true"><KeyRound /></span>
            <div>
              <p>ADMIN ACCESS</p>
              <h2 id="login-title">管理员登录</h2>
            </div>
          </div>

          <form onSubmit={submit}>
            <div className="field-group">
              <label htmlFor={usernameId}>用户名</label>
              <input
                id={usernameId}
                name="username"
                type="text"
                autoComplete="username"
                autoCapitalize="none"
                spellCheck="false"
                value={username}
                onChange={(event) => setUsername(event.target.value)}
                disabled={submitting}
                required
                autoFocus
              />
            </div>

            <div className="field-group">
              <label htmlFor={passwordId}>密码</label>
              <input
                id={passwordId}
                name="password"
                type="password"
                autoComplete="current-password"
                value={password}
                onChange={(event) => setPassword(event.target.value)}
                disabled={submitting}
                required
              />
            </div>

            <div className="login-feedback" aria-live="polite">
              {message ? <p className="login-error">{message}</p> : null}
              {setupRequired ? (
                <div className="setup-guide">
                  <span>在 Server 本机执行</span>
                  <code>xtunnel-server admin create --username admin --password-file /run/secrets/xtunnel-admin-password</code>
                  <small>密码请通过隐藏 TTY 或受保护的 password file 输入。</small>
                </div>
              ) : null}
            </div>

            <button className="login-submit" type="submit" disabled={submitting}>
              {submitting ? <LoaderCircle className="button-spinner" aria-hidden="true" /> : null}
              {submitting ? "正在验证…" : "进入管理控制台"}
            </button>
          </form>

          <p className="login-footnote">
            仅在受信任的 HTTPS 管理地址登录。XTunnel 不提供默认管理员密码。
          </p>
        </div>
      </section>
    </main>
  );
}

function Console({ session, onSessionExpired }: {
  session: AuthSession;
  onSessionExpired: (message?: string) => void;
}) {
  const [loggingOut, setLoggingOut] = useState(false);
  const [logoutError, setLogoutError] = useState<string>();
  const [dashboard, setDashboard] = useState<DashboardState>({ status: "loading" });
  const [currentPage, setCurrentPage] = useState<ConsolePage>("overview");

  async function loadDashboard(signal?: AbortSignal) {
    setDashboard({ status: "loading" });
    try {
      const result = await apiClient.GET("/dashboard", { signal });
      if (result.data) {
        setDashboard({ status: "ready", dashboard: result.data as Dashboard });
        return;
      }
      if (result.response.status === 401) {
        onSessionExpired("管理会话已过期，请重新登录。");
        return;
      }
      setDashboard({
        status: "error",
        message: apiErrorMessage(result.error as APIErrorEnvelope | undefined) ?? "无法读取运行状态。",
      });
    } catch (error: unknown) {
      if (!isAbortError(error)) {
        setDashboard({ status: "error", message: "无法连接管理服务，请稍后重试。" });
      }
    }
  }

  useEffect(() => {
    const controller = new AbortController();
    void loadDashboard(controller.signal);
    return () => controller.abort();
  }, []);

  async function logout() {
    setLoggingOut(true);
    setLogoutError(undefined);

    try {
      const result = await apiClient.POST("/auth/logout", {
        params: { header: { Origin: window.location.origin } },
        headers: { "X-XTunnel-CSRF": session.csrf_token },
      });

      if (result.response.status === 204 || result.response.status === 401) {
        onSessionExpired();
        return;
      }

      const error = result.error as APIErrorEnvelope | undefined;
      const code = apiErrorCode(error);
      setLogoutError(
        code === "CSRF_INVALID"
          ? "当前页面凭据已失效，请刷新页面后重试。"
          : apiErrorMessage(error) ?? "退出失败，请稍后重试。",
      );
    } catch {
      setLogoutError("无法连接管理服务，当前页面仍保持登录状态。");
    } finally {
      setLoggingOut(false);
    }
  }

  return (
    <div className="app-shell">
      <header className="topbar">
        <Brand />
        <div className="topbar-context">管理控制台</div>
        <div className="topbar-session">
          <span className="topbar-user">
            <small>当前管理员</small>
            <strong>{session.admin.username}</strong>
          </span>
          <button type="button" onClick={logout} disabled={loggingOut}>
            {loggingOut ? <LoaderCircle className="button-spinner" aria-hidden="true" /> : <LogOut aria-hidden="true" />}
            <span>{loggingOut ? "退出中" : "退出"}</span>
          </button>
        </div>
      </header>

      <aside className="sidebar">
        <nav aria-label="主导航">
          <p className="nav-heading">工作台</p>
          <div className="nav-list">
            {navigation.map((item) => {
              const Icon = item.icon;
              const active = item.page === currentPage;
              return (
                <button
                  type="button"
                  aria-current={active ? "page" : undefined}
                  className={`nav-item${active ? " active" : ""}`}
                  key={item.label}
                  disabled={!item.page}
                  onClick={() => item.page && setCurrentPage(item.page)}
                >
                  <Icon className="nav-icon" aria-hidden="true" />
                  {item.label}
                  {!item.page ? <span className="nav-pending">待接入</span> : null}
                </button>
              );
            })}
          </div>
        </nav>

        <div className="sidebar-footer">
          <span className="status-dot" aria-hidden="true" />
          管理会话已连接
        </div>
      </aside>

      <main className="main-content" id={currentPage}>
        {logoutError ? <div className="console-alert" role="alert">{logoutError}</div> : null}
        {currentPage === "overview" ? (
          <>
            <div className="page-heading">
              <p className="breadcrumb">工作台 / 概览</p>
              <h1>概览</h1>
              <p>所有状态均直接来自 Server 权威快照；控制台只负责呈现，不在浏览器内重新判定。</p>
            </div>

            {dashboard.status === "loading" ? (
              <section className="dashboard-loading" aria-live="polite" aria-busy="true">
                <LoaderCircle className="button-spinner" aria-hidden="true" />
                正在读取 Server 快照…
              </section>
            ) : null}

            {dashboard.status === "error" ? (
              <section className="dashboard-error" role="alert">
                <AlertTriangle aria-hidden="true" />
                <div>
                  <strong>运行状态暂不可用</strong>
                  <p>{dashboard.message}</p>
                </div>
                <button type="button" onClick={() => void loadDashboard()}>
                  <RefreshCw aria-hidden="true" />重新读取
                </button>
              </section>
            ) : null}

            {dashboard.status === "ready" ? (
              <DashboardView dashboard={dashboard.dashboard} onRefresh={() => void loadDashboard()} />
            ) : null}
          </>
        ) : (
          <ManagementView csrfToken={session.csrf_token} onSessionExpired={onSessionExpired} />
        )}
      </main>
    </div>
  );
}

function DashboardView({ dashboard, onRefresh }: {
  dashboard: Dashboard;
  onRefresh: () => void;
}) {
  const counts = dashboard.counts;
  const trafficAvailable = dashboard.traffic.availability === "AVAILABLE";
  const errorsAvailable = dashboard.recent_errors.availability === "AVAILABLE";

  return (
    <div className="dashboard-stack">
      <section className="control-spine" aria-labelledby="control-spine-title">
        <div className="section-heading">
          <div>
            <p className="section-kicker">CONTROL CHAIN</p>
            <h2 id="control-spine-title">控制链状态</h2>
          </div>
          <div className="snapshot-meta">
            <span>{new Date(dashboard.generated_at).toLocaleString("zh-CN", { hour12: false })}</span>
            <button type="button" onClick={onRefresh} aria-label="刷新运行状态">
              <RefreshCw aria-hidden="true" />刷新
            </button>
          </div>
        </div>

        <div className="spine-track">
          <article className={`spine-node server-node ${dashboard.server_status.toLowerCase()}`}>
            <span className="spine-icon"><Server aria-hidden="true" /></span>
            <div>
              <small>SERVER</small>
              <strong>{dashboard.server_status}</strong>
              <p>管理面权威状态</p>
            </div>
          </article>
          <span className="spine-link" aria-hidden="true" />
          <article className="spine-node">
            <span className="spine-icon"><Network aria-hidden="true" /></span>
            <div>
              <small>TUNNEL</small>
              <strong>{counts.tunnels_online} / {counts.tunnels_total}</strong>
              <p>在线 · 离线 {counts.tunnels_offline}</p>
            </div>
          </article>
          <span className="spine-link" aria-hidden="true" />
          <article className="spine-node">
            <span className="spine-icon"><Cable aria-hidden="true" /></span>
            <div>
              <small>CONNECTOR</small>
              <strong>{counts.connectors_online}</strong>
              <p>当前在线</p>
            </div>
          </article>
          <span className="spine-link" aria-hidden="true" />
          <article className="spine-node">
            <span className="spine-icon"><CircleCheck aria-hidden="true" /></span>
            <div>
              <small>SERVICE</small>
              <strong>{counts.services_ready} / {counts.services_total}</strong>
              <p>就绪 · 应用失败 {counts.services_error}</p>
            </div>
          </article>
        </div>
      </section>

      <section className="operations-grid" aria-label="运行指标">
        <article className="operations-panel active-connections">
          <div className="metric-heading">
            <span><Activity aria-hidden="true" />实时连接</span>
            <small>LIVE</small>
          </div>
          <strong className="metric-value">{counts.active_connections.toLocaleString("zh-CN")}</strong>
          <p>当前活跃连接，由 Server 快照直接返回。</p>
        </article>

        <article className="operations-panel traffic-panel">
          <div className="metric-heading">
            <span><RadioTower aria-hidden="true" />今日流量</span>
            <small>{dashboard.traffic.availability}</small>
          </div>
          {trafficAvailable ? (
            <dl className="traffic-values">
              <div><dt>连接</dt><dd>{dashboard.traffic.connections_today?.toLocaleString("zh-CN")}</dd></div>
              <div><dt>入站</dt><dd>{formatBytes(dashboard.traffic.ingress_bytes_today)}</dd></div>
              <div><dt>出站</dt><dd>{formatBytes(dashboard.traffic.egress_bytes_today)}</dd></div>
            </dl>
          ) : (
            <div className="unavailable-state">
              <span>—</span>
              <p>M6 Usage Read Model 尚未接入，未将缺失数据伪造为 0。</p>
            </div>
          )}
        </article>

        <article className="operations-panel recent-errors">
          <div className="metric-heading">
            <span><AlertTriangle aria-hidden="true" />最近错误</span>
            <small>{dashboard.recent_errors.availability}</small>
          </div>
          {errorsAvailable && dashboard.recent_errors.items.length > 0 ? (
            <ul className="error-list">
              {dashboard.recent_errors.items.map((item) => (
                <li key={`${item.occurred_at}-${item.code}`}>
                  <strong>{item.code}</strong>
                  <span>{item.message}</span>
                  <time dateTime={item.occurred_at}>{new Date(item.occurred_at).toLocaleString("zh-CN", { hour12: false })}</time>
                </li>
              ))}
            </ul>
          ) : (
            <div className="unavailable-state compact">
              <span>—</span>
              <p>{errorsAvailable ? "当前快照没有最近错误。" : "M6 Error Read Model 尚未接入。"}</p>
            </div>
          )}
        </article>
      </section>
    </div>
  );
}

function formatBytes(value: number | null) {
  if (value === null) {
    return "—";
  }
  if (value < 1024) {
    return `${value} B`;
  }
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let amount = value / 1024;
  let unit = units[0];
  for (let index = 1; index < units.length && amount >= 1024; index += 1) {
    amount /= 1024;
    unit = units[index];
  }
  return `${amount.toFixed(amount >= 10 ? 1 : 2)} ${unit}`;
}

export function App() {
  const [auth, setAuth] = useState<AuthState>({ status: "checking" });

  useEffect(() => {
    const controller = new AbortController();

    void apiClient.GET("/auth/me", { signal: controller.signal })
      .then((result) => {
        if (result.data) {
          setAuth({ status: "authenticated", session: result.data });
          return;
        }
        if (result.response.status === 401) {
          setAuth({ status: "anonymous" });
          return;
        }
        setAuth({
          status: "anonymous",
          message: apiErrorMessage(result.error as APIErrorEnvelope | undefined) ?? "无法确认当前会话，请重新登录。",
        });
      })
      .catch((error: unknown) => {
        if (!isAbortError(error)) {
          setAuth({ status: "anonymous", message: "无法确认当前会话，请重新登录。" });
        }
      });

    return () => controller.abort();
  }, []);

  if (auth.status === "checking") {
    return <SessionCheck />;
  }
  if (auth.status === "anonymous") {
    return (
      <LoginPage
        initialMessage={auth.message}
        onLogin={(session) => setAuth({ status: "authenticated", session })}
      />
    );
  }
  return (
    <Console
      session={auth.session}
      onSessionExpired={(message) => setAuth({ status: "anonymous", message })}
    />
  );
}
