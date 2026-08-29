import {
  Bot,
  KeyRound,
  LayoutDashboard,
  LoaderCircle,
  LockKeyhole,
  LogOut,
  Network,
  RadioTower,
  Server,
  Settings,
  ShieldCheck,
} from "lucide-react";
import { useEffect, useId, useState } from "react";
import type { FormEvent } from "react";

import { apiClient } from "./api/client";
import type { components } from "./api/schema.gen";

type AuthSession = components["schemas"]["AuthSession"];
type APIErrorEnvelope =
  | components["schemas"]["ErrorResponse"]
  | components["schemas"]["SetupRequiredErrorResponse"];

type AuthState =
  | { status: "checking" }
  | { status: "anonymous"; message?: string }
  | { status: "authenticated"; session: AuthSession };

const navigation = [
  { label: "概览", icon: LayoutDashboard },
  { label: "Agent 管理", icon: Bot },
  { label: "服务与隧道", icon: Network },
  { label: "访问入口", icon: RadioTower },
  { label: "系统设置", icon: Settings },
] as const;

const foundationStatus = [
  ["管理认证", "已启用", "管理员 Session 与 CSRF 保护已接入"],
  ["同源代理", "已就绪", "/api/v1 保持浏览器 Host 与 Origin"],
  ["生产构建", "已嵌入", "静态资源随 Server Binary 一起交付"],
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
            {navigation.map((item, index) => {
              const Icon = item.icon;
              return (
                <div
                  aria-current={index === 0 ? "page" : undefined}
                  aria-disabled={index === 0 ? undefined : "true"}
                  className={`nav-item${index === 0 ? " active" : ""}`}
                  key={item.label}
                >
                  <Icon className="nav-icon" aria-hidden="true" />
                  {item.label}
                  {index !== 0 ? <span className="nav-pending">待接入</span> : null}
                </div>
              );
            })}
          </div>
        </nav>

        <div className="sidebar-footer">
          <span className="status-dot" aria-hidden="true" />
          管理会话已连接
        </div>
      </aside>

      <main className="main-content" id="overview">
        {logoutError ? <div className="console-alert" role="alert">{logoutError}</div> : null}
        <div className="page-heading">
          <p className="breadcrumb">工作台 / 概览</p>
          <h1>概览</h1>
          <p>查看 XTunnel 管理面的接入状态。其余业务页面将在对应 REST API 完成后开放。</p>
        </div>

        <section className="status-grid" aria-label="工程基础状态">
          {foundationStatus.map(([label, state, detail]) => (
            <article className="status-card" key={label}>
              <div className="status-card-heading">
                <h2>{label}</h2>
                <span>{state}</span>
              </div>
              <p>{detail}</p>
            </article>
          ))}
        </section>

        <section className="data-panel" aria-labelledby="resources-title">
          <div className="panel-heading">
            <div>
              <h2 id="resources-title">资源状态</h2>
              <p>Agent、服务与隧道的真实运行数据将在接口接入后展示。</p>
            </div>
            <span className="panel-state">等待业务 API</span>
          </div>

          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">资源名称</th>
                  <th scope="col">资源类型</th>
                  <th scope="col">运行状态</th>
                  <th scope="col">最近更新</th>
                </tr>
              </thead>
              <tbody>
                <tr>
                  <td colSpan={4}>
                    <div className="empty-state">
                      <span className="empty-icon" aria-hidden="true" />
                      <strong>暂无资源数据</strong>
                      <span>当前已完成管理认证，不展示尚未接入的推测数据。</span>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>
      </main>
    </div>
  );
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
