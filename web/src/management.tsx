import {
  AlertTriangle,
  ArrowLeft,
  Cable,
  Check,
  ChevronRight,
  CirclePlus,
  Clipboard,
  KeyRound,
  LoaderCircle,
  Pencil,
  Power,
  PowerOff,
  RefreshCw,
  RotateCw,
  ServerCog,
  Search,
  ShieldOff,
  Trash2,
  X,
} from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";
import type { ReactNode } from "react";

import { apiClient } from "./api/client";
import type { components } from "./api/schema.gen";
import "./management.css";

type Tunnel = components["schemas"]["Tunnel"];
type Connector = components["schemas"]["Connector"];
type Service = components["schemas"]["Service"];
type ConnectionCredential = components["schemas"]["ConnectionCredential"];
type CreateServiceRequest = components["schemas"]["CreateServiceRequest"];
type UpdateServiceRequest = components["schemas"]["UpdateServiceRequest"];
type APIErrorEnvelope = components["schemas"]["ErrorResponse"];

type WorkspaceState =
  | { status: "idle" }
  | { status: "loading" }
  | {
    status: "ready";
    tunnel: Tunnel;
    tunnelETag: string;
    connectors: Connector[];
    connectorPageToken?: string;
    services: Service[];
    servicePageToken?: string;
  }
  | { status: "error"; message: string };

type DialogState =
  | { kind: "create-tunnel" }
  | { kind: "install-tunnel"; tunnel: Tunnel; credential: ConnectionCredential }
  | { kind: "rename-tunnel"; tunnel: Tunnel; etag: string }
  | { kind: "create-service"; tunnel: Tunnel; etag: string }
  | { kind: "edit-service"; service: Service; etag: string }
  | { kind: "credential"; credential: ConnectionCredential; title: string; returnFocus?: HTMLElement }
  | { kind: "confirm"; title: string; message: string; actionLabel: string; danger?: boolean; run: () => Promise<void> };

type Feedback = { tone: "success" | "error"; message: string };

const PAGE_SIZE = 200;

function errorMessage(error: unknown, fallback: string) {
  const envelope = error as APIErrorEnvelope | undefined;
  return envelope?.error.message ?? fallback;
}

function formatDate(value: string | null) {
  if (!value) {
    return "—";
  }
  return new Date(value).toLocaleString("zh-CN", { hour12: false });
}

function statusLabel(status: string) {
  const labels: Record<string, string> = {
    ACTIVE: "有效",
    APPLY_FAILED: "应用失败",
    CONFIG_SYNCING: "同步中",
    DEGRADED: "降级",
    DISABLED: "已禁用",
    DRAINING: "排空中",
    NO_CAPACITY: "容量不足",
    OFFLINE: "离线",
    ONLINE: "在线",
    ORIGIN_UNHEALTHY: "源站异常",
    READY: "就绪",
    REVOKED: "已撤销",
    TUNNEL_OFFLINE: "Tunnel 离线",
  };
  return labels[status] ?? status;
}

function StatusBadge({ status }: { status: string }) {
  return <span className={`resource-status status-${status.toLowerCase().replaceAll("_", "-")}`}>{statusLabel(status)}</span>;
}

function Dialog({ title, eyebrow, children, onClose, drawer = false, returnFocus }: {
  title: string;
  eyebrow: string;
  drawer?: boolean;
  returnFocus?: HTMLElement;
  children: ReactNode;
  onClose: () => void;
}) {
  const dialogRef = useRef<HTMLElement>(null);
  const onCloseRef = useRef(onClose);
  const previousFocusRef = useRef<HTMLElement | null>(
    returnFocus ?? (document.activeElement instanceof HTMLElement ? document.activeElement : null),
  );
  onCloseRef.current = onClose;

  useEffect(() => {
    const element = dialogRef.current;
    const focusable = () => element
      ? [...element.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled), select:not(:disabled), [tabindex]:not([tabindex="-1"])')]
      : [];
    (element?.querySelector<HTMLElement>("[data-dialog-initial-focus]") ?? focusable()[0])?.focus();

    function keyDown(event: KeyboardEvent) {
      if (event.key === "Escape") {
        event.preventDefault();
        onCloseRef.current();
        return;
      }
      if (event.key !== "Tab") return;
      const items = focusable();
      if (items.length === 0) return;
      const first = items[0];
      const last = items[items.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    }

    document.addEventListener("keydown", keyDown);
    return () => {
      document.removeEventListener("keydown", keyDown);
      previousFocusRef.current?.focus();
    };
  }, []);

  return (
    <div className={`dialog-backdrop${drawer ? " drawer-backdrop" : ""}`} role="presentation" onMouseDown={(event) => {
      if (event.target === event.currentTarget) {
        onClose();
      }
    }}>
      <section ref={dialogRef} className={`management-dialog${drawer ? " connector-drawer" : ""}`} role="dialog" aria-modal="true" aria-labelledby="management-dialog-title" tabIndex={-1}>
        <header>
          <div>
            <p>{eyebrow}</p>
            <h2 id="management-dialog-title">{title}</h2>
          </div>
          <button className="icon-button" type="button" onClick={onClose} aria-label="关闭">
            <X aria-hidden="true" />
          </button>
        </header>
        {children}
      </section>
    </div>
  );
}

function ConfirmDialog({ dialog, busy, onClose }: {
  dialog: Extract<DialogState, { kind: "confirm" }>;
  busy: boolean;
  onClose: () => void;
}) {
  return (
    <Dialog title={dialog.title} eyebrow="CONFIRM ACTION" onClose={onClose}>
      <div className="confirm-dialog-body">
        <AlertTriangle aria-hidden="true" />
        <p>{dialog.message}</p>
      </div>
      <footer className="dialog-actions">
        <button className="secondary-button" type="button" onClick={onClose} disabled={busy} data-dialog-initial-focus>取消</button>
        <button className={dialog.danger ? "danger-button" : "primary-button"} type="button" onClick={() => void dialog.run()} disabled={busy}>
          {busy ? <LoaderCircle className="button-spinner" aria-hidden="true" /> : null}
          {dialog.actionLabel}
        </button>
      </footer>
    </Dialog>
  );
}

function FlowPage({ title, onClose, busy = false, step, children }: {
  title: string;
  onClose: () => void;
  busy?: boolean;
  step?: 1 | 2;
  children: ReactNode;
}) {
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => { heading.current?.focus(); }, [title]);
  return (
    <section className="creation-page" aria-label={title}>
      <header className="creation-page-heading">
        <button className="text-button" type="button" onClick={onClose} disabled={busy}><ArrowLeft aria-hidden="true" />返回隧道</button>
        <p className="breadcrumb">工作台 / 服务与隧道 / {title}</p>
        <h1 ref={heading} tabIndex={-1}>{title}</h1>
      </header>
      {step ? <ol className="creation-steps" aria-label="创建隧道步骤">
        <li aria-current={step === 1 ? "step" : undefined} className={step === 1 ? "current" : "complete"}><span>{step === 2 ? <Check aria-hidden="true" /> : "1"}</span>命名隧道</li>
        <li aria-current={step === 2 ? "step" : undefined} className={step === 2 ? "current" : ""}><span>2</span>安装连接器</li>
      </ol> : null}
      {children}
    </section>
  );
}

function TunnelNameDialog({ mode, initialName = "", busy, onClose, onSubmit }: {
  mode: "create" | "rename";
  initialName?: string;
  busy: boolean;
  onClose: () => void;
  onSubmit: (name: string) => Promise<void>;
}) {
  const nameId = useId();
  const [name, setName] = useState(initialName);

  const content = (
      <form className={mode === "create" ? "tunnel-name-form detail-card" : undefined} onSubmit={(event) => {
        event.preventDefault();
        void onSubmit(name.trim());
      }}>
        <div className="field-group">
          <label htmlFor={nameId}>名称</label>
          <input id={nameId} value={name} onChange={(event) => setName(event.target.value)} disabled={busy} required data-dialog-initial-focus />
          <small>用于在控制台识别这条 Tunnel，不影响 Agent Token。</small>
        </div>
        <footer className="dialog-actions">
          <button className="secondary-button" type="button" onClick={onClose} disabled={busy}>取消</button>
          <button className="primary-button" type="submit" disabled={busy || !name.trim()}>
            {busy ? <LoaderCircle className="button-spinner" aria-hidden="true" /> : null}
            {mode === "create" ? "创建并继续" : "保存名称"}
          </button>
        </footer>
      </form>
  );
  return mode === "create"
    ? <FlowPage title="创建 Tunnel" onClose={onClose} busy={busy} step={1}>{content}</FlowPage>
    : <Dialog title="重命名 Tunnel" eyebrow="TUNNEL IDENTITY" onClose={onClose}>{content}</Dialog>;
}

function CredentialContent({ credential }: { credential: ConnectionCredential }) {
  const [copied, setCopied] = useState<string>();
  const [platform, setPlatform] = useState<"windows" | "linux" | "docker">("windows");
  const [mode, setMode] = useState<"service" | "foreground">("service");
  const copyTimer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(copyTimer.current), []);

  const environment = platform === "docker" ? "CONTAINER"
    : mode === "foreground" ? "FOREGROUND"
      : platform === "windows" ? "WINDOWS_SCM" : "LINUX_SYSTEMD";
  const sourceCommand = credential.deployment_commands.find((item) => item.environment === environment)?.command;
  const command = platform === "windows" && mode === "foreground"
    ? sourceCommand?.replace(/^xtunnel-agent\b/, ".\\xtunnel-agent.exe") : sourceCommand;
  const platformLabel = platform === "windows" ? "Windows" : platform === "linux" ? "Linux" : "Docker";
  const modeLabel = platform === "docker" ? "容器运行" : mode === "service" ? "安装为系统服务" : "前台运行";

  async function copy(label: string, value: string) {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(label);
      window.clearTimeout(copyTimer.current);
      copyTimer.current = window.setTimeout(() => setCopied(undefined), 1600);
    } catch {
      setCopied(`error:${label}`);
    }
  }

  return (
      <div className="connector-guide-content">
        <div className="connector-guide-intro">
          <span className="connector-guide-icon"><Cable aria-hidden="true" /></span>
          <h3>将设备连接到这条 Tunnel</h3>
          <p>在能访问源站的设备上运行 XTunnel Agent。连接成功后，可在控制台查看 Connector 状态并配置 Service。</p>
        </div>
        <section className="connector-install-step" aria-labelledby="connector-platform-heading">
          <h3 id="connector-platform-heading"><span>1</span>选择运行环境</h3>
          <div className="connector-platforms" role="group" aria-label="操作系统">
            {(["windows", "linux", "docker"] as const).map((value) => (
              <button type="button" key={value} aria-pressed={platform === value} onClick={() => { setPlatform(value); setCopied(undefined); }}>
                <ServerCog aria-hidden="true" />
                {value === "windows" ? "Windows" : value === "linux" ? "Linux" : "Docker"}
                {platform === value ? <Check aria-hidden="true" /> : null}
              </button>
            ))}
          </div>
          {platform !== "docker" ? <div className="segmented-control connector-run-mode" role="group" aria-label="运行方式">
            <button type="button" className={mode === "service" ? "active" : ""} aria-pressed={mode === "service"} onClick={() => { setMode("service"); setCopied(undefined); }}>系统服务</button>
            <button type="button" className={mode === "foreground" ? "active" : ""} aria-pressed={mode === "foreground"} onClick={() => { setMode("foreground"); setCopied(undefined); }}>前台运行</button>
          </div> : null}
        </section>
        <section className="connector-install-step" aria-labelledby="connector-command-heading">
          <h3 id="connector-command-heading"><span>2</span>复制并执行命令</h3>
          <p>{platform === "windows"
            ? mode === "service" ? "准备好 xtunnel-agent.exe，在其所在目录打开管理员 PowerShell，执行以下命令。" : "准备好 xtunnel-agent.exe，在其所在目录打开 PowerShell。保持终端运行以维持连接。"
            : platform === "linux" ? mode === "service" ? "将 xtunnel-agent 安装到 PATH，在支持 systemd 的主机上执行以下命令。" : "将 xtunnel-agent 安装到 PATH，执行以下命令并保持进程运行。"
              : "准备好 Docker 和 xtunnel-agent:v0.1.0 镜像，在能访问源站的容器网络中执行以下命令。"}</p>
          {command ? <div className="deployment-list">
            <article>
              <div><strong>{platformLabel} · {modeLabel}</strong><code>{command}</code></div>
              <button type="button" onClick={() => void copy("command", command)}>
                {copied === "command" ? <Check aria-hidden="true" /> : <Clipboard aria-hidden="true" />}
                {copied === "command" ? "已复制" : copied === "error:command" ? "复制失败" : "复制命令"}
              </button>
            </article>
          </div> : <p role="status">当前环境的部署命令不可用，请选择其他运行环境。</p>}
        </section>
        <section className="connector-install-step" aria-labelledby="connector-token-heading">
          <h3 id="connector-token-heading"><span>3</span>连接凭据</h3>
          <p>Token 已加密保存在服务端。以后可从“安装连接器”再次查看，关闭指引不会删除 Token。Token 包含签发时的 Gateway 地址、端口和 TLS 信任信息。</p>
          <div className="credential-token">
            <div><span>Connection Token · v{credential.token_version}</span><code>{credential.connection_token}</code></div>
            <button type="button" onClick={() => void copy("token", credential.connection_token)}>
              {copied === "token" ? <Check aria-hidden="true" /> : <Clipboard aria-hidden="true" />}
              {copied === "token" ? "已复制" : copied === "error:token" ? "复制失败" : "复制 Token"}
            </button>
          </div>
          <p className="connector-token-hint"><KeyRound aria-hidden="true" />Token 用于授权设备连接，请仅提供给可信设备。</p>
        </section>
      </div>
  );
}

function CredentialDialog({ credential, title, onClose, returnFocus }: {
  credential: ConnectionCredential;
  title: string;
  onClose: () => void;
  returnFocus?: HTMLElement;
}) {
  return (
    <Dialog title={title} eyebrow="CONNECT YOUR TUNNEL" onClose={onClose} drawer returnFocus={returnFocus}>
      <CredentialContent credential={credential} />
      <footer className="dialog-actions connector-guide-footer">
        <span>可随时重新打开此安装指引</span>
        <button className="primary-button" type="button" onClick={onClose}>完成</button>
      </footer>
    </Dialog>
  );
}
function TunnelInstallation({ tunnel, credential, onClose, onAddService, onSessionExpired, busy }: {
  tunnel: Tunnel;
  credential: ConnectionCredential;
  onClose: () => void;
  onAddService: () => void;
  onSessionExpired: (message?: string) => void;
  busy: boolean;
}) {
  const [connectors, setConnectors] = useState<Connector[]>([]);
  const [refreshError, setRefreshError] = useState<string>();
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    let timer: number | undefined;
    // 安装步骤拥有轮询：请求结束才安排下一次，卸载时取消 IO 与计时器。
    // Token 只来自创建响应，刷新 Connector 不会再次签发或 Reveal 凭据。
    async function refresh() {
      try {
        const nextConnectors: Connector[] = [];
        let pageToken: string | undefined;
        do {
          const result = await apiClient.GET("/tunnels/{tunnel_id}/connectors", {
            signal: controller.signal,
            params: { path: { tunnel_id: tunnel.id }, query: { page_size: PAGE_SIZE, ...(pageToken ? { page_token: pageToken } : {}) } },
          });
          if (controller.signal.aborted) return;
          if (result.response.status === 401) {
            onSessionExpired("管理会话已过期，请重新登录。");
            return;
          }
          if (!result.data) {
            throw new Error(errorMessage(result.error, "无法刷新连接器，将自动重试。"));
          }
          nextConnectors.push(...(result.data.items as ReadonlyArray<Connector>));
          pageToken = result.data.next_page_token;
        } while (pageToken);
        // 每轮读完全部页后整体替换，断连项自然移除；中途失败不发布半轮列表。
        setConnectors(nextConnectors);
        setLoaded(true);
        setRefreshError(undefined);
      } catch (error: unknown) {
        if (controller.signal.aborted) return;
        setRefreshError(error instanceof Error ? error.message : "无法连接管理服务，将自动重试。");
      }
      if (!controller.signal.aborted) timer = window.setTimeout(() => void refresh(), 3000);
    }
    void refresh();
    return () => { controller.abort(); window.clearTimeout(timer); };
  }, [tunnel.id, onSessionExpired]);
  return (
    <FlowPage title="安装连接器" onClose={onClose} busy={busy} step={2}>
      <p className="creation-description">隧道 <strong>{tunnel.name}</strong> 已创建。在源站设备运行连接器，然后添加要发布的服务。</p>
      <div className="installation-content detail-card"><CredentialContent credential={credential} /></div>
      <section className="installation-connectors detail-card" aria-label="已连接连接器">
        <header className="detail-card-heading"><div><h2>已连接连接器</h2><p>每 3 秒自动刷新 · 连接器断开后会从列表移除</p></div><span className="item-count">{connectors.length} 个连接器</span></header>
        {refreshError ? <p role="alert" className="installation-refresh-error">{refreshError} 已显示的列表保留供参考。</p> : null}
        <div className="resource-table-scroll" tabIndex={0} role="region" aria-label="安装进度表格滚动区域">
          <table className="resource-table" aria-label="安装进度连接器">
            <thead><tr><th>连接器</th><th>状态</th><th>系统 / 架构</th><th>版本</th><th>连接时间</th></tr></thead>
            <tbody>{connectors.map((connector) => <tr key={connector.id}><td><strong>{connector.hostname}</strong><small>{connector.id}</small></td><td><StatusBadge status={connector.status} /></td><td>{connector.os} / {connector.arch}</td><td>{connector.version}</td><td>{formatDate(connector.connected_at)}</td></tr>)}</tbody>
          </table>
        </div>
        {!connectors.length ? <p className="installation-waiting" role="status">{refreshError ? "正在等待下一次刷新。" : loaded ? "等待连接器连接。请在设备上执行上方命令。" : "正在读取连接器状态…"}</p> : null}
      </section>
      <footer className="dialog-actions creation-actions"><span>隧道已保存，可稍后继续配置。</span><button className="secondary-button" type="button" onClick={onClose} disabled={busy}>完成</button><button className="primary-button" type="button" onClick={onAddService} disabled={busy}>下一步：添加服务<ChevronRight aria-hidden="true" /></button></footer>
    </FlowPage>
  );
}

type ServiceFormValues = {
  name: string;
  originScheme: "http" | "https" | "tcp";
  originHost: string;
  originPort: string;
  connectTimeout: string;
  tlsVerify: boolean;
  tlsServerName: string;
  httpHostHeader: string;
  exposureType: "none" | "http" | "tcp";
  publicHostname: string;
  pathPrefix: string;
  preserveHost: boolean;
  publicPort: string;
  healthType: "none" | "TCP" | "HTTP";
  healthPath: string;
  enabled: boolean;
};

function serviceFormValues(service?: Service): ServiceFormValues {
  const origin = service?.origin;
  const exposure = service?.exposure;
  const health = service?.health;
  return {
    name: service?.name ?? "",
    originScheme: origin?.scheme ?? "http",
    originHost: origin?.host ?? "127.0.0.1",
    originPort: String(origin?.port ?? 8080),
    connectTimeout: String(origin?.connect_timeout_ms ?? 5000),
    tlsVerify: origin?.scheme === "https" ? origin.tls_verify : true,
    tlsServerName: origin?.scheme === "https" ? (origin.tls_server_name ?? "") : "",
    httpHostHeader: origin && origin.scheme !== "tcp" ? (origin.http_host_header ?? "") : "",
    exposureType: exposure?.type ?? (service ? "none" : "http"),
    publicHostname: exposure?.type === "http" ? exposure.hostname : "",
    pathPrefix: exposure?.type === "http" ? exposure.path_prefix : "/",
    preserveHost: exposure?.type === "http" ? exposure.preserve_host : true,
    publicPort: exposure?.type === "tcp" ? String(exposure.public_port) : "",
    healthType: health?.type ?? "none",
    healthPath: health?.type === "HTTP" ? health.path : "/health",
    enabled: service?.enabled ?? true,
  };
}

function ServiceDialog({ dialog, busy, onClose, onSubmit }: {
  dialog: Extract<DialogState, { kind: "create-service" | "edit-service" }>;
  busy: boolean;
  onClose: () => void;
  onSubmit: (body: CreateServiceRequest | UpdateServiceRequest) => Promise<void>;
}) {
  const editing = dialog.kind === "edit-service";
  const [values, setValues] = useState(() => serviceFormValues(editing ? dialog.service : undefined));

  function field<K extends keyof ServiceFormValues>(key: K, value: ServiceFormValues[K]) {
    setValues((current) => ({ ...current, [key]: value }));
  }

  function buildBody() {
    const port = Number(values.originPort);
    const timeout = Number(values.connectTimeout);
    const origin = values.originScheme === "https"
      ? {
        scheme: "https" as const,
        host: values.originHost.trim(),
        port,
        connect_timeout_ms: timeout,
        tls_verify: values.tlsVerify,
        ...(values.tlsServerName.trim() ? { tls_server_name: values.tlsServerName.trim() } : {}),
        ...(values.httpHostHeader.trim() ? { http_host_header: values.httpHostHeader.trim() } : {}),
      }
      : values.originScheme === "http"
        ? {
          scheme: "http" as const,
          host: values.originHost.trim(),
          port,
          connect_timeout_ms: timeout,
          ...(values.httpHostHeader.trim() ? { http_host_header: values.httpHostHeader.trim() } : {}),
        }
        : { scheme: "tcp" as const, host: values.originHost.trim(), port, connect_timeout_ms: timeout };

    const exposure = values.exposureType === "none"
      ? null
      : values.exposureType === "http"
        ? {
          type: "http" as const,
          hostname: values.publicHostname.trim(),
          path_prefix: values.pathPrefix.trim() || "/",
          preserve_host: values.preserveHost,
        }
        : {
          type: "tcp" as const,
          ...(values.publicPort.trim() ? { public_port: Number(values.publicPort) } : {}),
        };

    const existingHealthType = editing ? dialog.service.health?.type : undefined;
    const healthPatch = values.healthType === "none"
      ? null
      : values.healthType === "HTTP"
        ? {
          type: "HTTP" as const,
          path: values.healthPath.trim() || "/health",
          ...(existingHealthType !== "HTTP" ? {
            interval_ms: 10000,
            timeout_ms: 2000,
            expected_status_min: 200,
            expected_status_max: 399,
            failure_threshold: 3,
            success_threshold: 2,
          } : {}),
        }
        : {
          type: "TCP" as const,
          ...(existingHealthType !== "TCP" ? {
            interval_ms: 10000,
            timeout_ms: 2000,
            failure_threshold: 3,
            success_threshold: 2,
          } : {}),
        };

    if (editing) {
      return { name: values.name.trim(), origin, exposure, health: healthPatch } satisfies UpdateServiceRequest;
    }
    if (!exposure) {
      throw new Error("创建 Service 必须配置一个公网 Exposure。");
    }
    const health = values.healthType === "none"
      ? null
      : values.healthType === "HTTP"
        ? {
          type: "HTTP" as const,
          path: values.healthPath.trim() || "/health",
          interval_ms: 10000,
          timeout_ms: 2000,
          expected_status_min: 200,
          expected_status_max: 399,
          failure_threshold: 3,
          success_threshold: 2,
        }
        : {
          type: "TCP" as const,
          interval_ms: 10000,
          timeout_ms: 2000,
          failure_threshold: 3,
          success_threshold: 2,
        };
    return {
      tunnel_id: dialog.tunnel.id,
      name: values.name.trim(),
      origin,
      exposure,
      health,
      enabled: values.enabled,
    } satisfies CreateServiceRequest;
  }

  return (
    <FlowPage title={editing ? "编辑 Service" : "创建 Service"} onClose={onClose} busy={busy}>
      {dialog.kind === "create-service" ? <p className="creation-description">为隧道 <strong>{dialog.tunnel.name}</strong> 添加服务。连接器将把公网请求转发到下方配置的源站。</p> : null}
      <form className="service-form" onSubmit={(event) => {
        event.preventDefault();
        void onSubmit(buildBody());
      }}>
        <section className="form-section">
          <h3>基本信息</h3>
          <div className="field-group">
            <label htmlFor="service-name">名称</label>
            <input id="service-name" value={values.name} onChange={(event) => field("name", event.target.value)} required disabled={busy} data-dialog-initial-focus />
          </div>
          {!editing ? (
            <label className="switch-field">
              <input type="checkbox" checked={values.enabled} onChange={(event) => field("enabled", event.target.checked)} disabled={busy} />
              <span>创建后立即启用</span>
            </label>
          ) : null}
        </section>

        <section className="form-section">
          <h3>公网入口</h3>
          <p className="form-hint">选择用户访问服务的入口。HTTP 域名需自行解析到 Server 的公网地址。</p>
          <div className="segmented-control" role="group" aria-label="公网入口类型">
            {editing ? <button type="button" className={values.exposureType === "none" ? "active" : ""} onClick={() => field("exposureType", "none")} aria-pressed={values.exposureType === "none"} disabled={busy}>不暴露</button> : null}
            <button type="button" className={values.exposureType === "http" ? "active" : ""} onClick={() => field("exposureType", "http")} aria-pressed={values.exposureType === "http"} disabled={busy}>HTTP</button>
            <button type="button" className={values.exposureType === "tcp" ? "active" : ""} onClick={() => field("exposureType", "tcp")} aria-pressed={values.exposureType === "tcp"} disabled={busy}>TCP</button>
          </div>
          {values.exposureType === "http" ? (
            <div className="form-grid two-columns">
              <div className="field-group">
                <label htmlFor="public-hostname">公网域名</label>
                <input id="public-hostname" value={values.publicHostname} onChange={(event) => field("publicHostname", event.target.value)} required disabled={busy} />
              </div>
              <div className="field-group">
                <label htmlFor="path-prefix">Path Prefix</label>
                <input id="path-prefix" value={values.pathPrefix} onChange={(event) => field("pathPrefix", event.target.value)} pattern="/.*" required disabled={busy} />
                <small>按路径前缀匹配，/ 表示全部路径。</small>
              </div>
              <label className="switch-field">
                <input type="checkbox" checked={values.preserveHost} onChange={(event) => field("preserveHost", event.target.checked)} disabled={busy} />
                <span>保留公网 Host</span>
              </label>
            </div>
          ) : null}
          {values.exposureType === "tcp" ? (
            <div className="field-group compact-field">
              <label htmlFor="public-port">公网端口（留空自动分配）</label>
              <input id="public-port" type="number" min="1" max="65535" value={values.publicPort} onChange={(event) => field("publicPort", event.target.value)} disabled={busy} />
            </div>
          ) : null}
          {values.exposureType === "none" ? <p className="form-hint">保存后移除当前 Public Exposure，Service 配置仍保留。</p> : null}
        </section>

        <section className="form-section">
          <h3>源站服务</h3>
          <p className="form-hint">填写连接器所在设备可访问的源站地址。</p>
          <div className="form-grid three-columns">
            <div className="field-group">
              <span className="field-label">协议</span>
              <div className="segmented-control" role="group" aria-label="Origin 协议">
                {(["http", "https", "tcp"] as const).map((scheme) => (
                  <button
                    type="button"
                    className={values.originScheme === scheme ? "active" : ""}
                    onClick={() => {
                      field("originScheme", scheme);
                      if (scheme === "tcp" && values.healthType === "HTTP") field("healthType", "TCP");
                    }}
                    aria-pressed={values.originScheme === scheme}
                    disabled={busy}
                    key={scheme}
                  >
                    {scheme.toUpperCase()}
                  </button>
                ))}
              </div>
            </div>
            <div className="field-group wide-field">
              <label htmlFor="origin-host">主机</label>
              <input id="origin-host" value={values.originHost} onChange={(event) => field("originHost", event.target.value)} required disabled={busy} />
            </div>
            <div className="field-group">
              <label htmlFor="origin-port">端口</label>
              <input id="origin-port" type="number" min="1" max="65535" value={values.originPort} onChange={(event) => field("originPort", event.target.value)} required disabled={busy} />
            </div>
          </div>
        </section>

        <details className="service-advanced" onInvalidCapture={(event) => { event.currentTarget.open = true; }}>
          <summary>高级设置</summary>
          <section className="form-section">
            <h3>源站连接选项</h3>
          <div className="form-grid two-columns">
            <div className="field-group">
              <label htmlFor="connect-timeout">连接超时（毫秒）</label>
              <input id="connect-timeout" type="number" min="1" value={values.connectTimeout} onChange={(event) => field("connectTimeout", event.target.value)} required disabled={busy} />
            </div>
            {values.originScheme !== "tcp" ? (
              <div className="field-group">
                <label htmlFor="host-header">Origin Host Header（可选）</label>
                <input id="host-header" value={values.httpHostHeader} onChange={(event) => field("httpHostHeader", event.target.value)} disabled={busy} />
              </div>
            ) : null}
          </div>
          {values.originScheme === "https" ? (
            <div className="form-grid two-columns">
              <label className="switch-field">
                <input type="checkbox" checked={values.tlsVerify} onChange={(event) => field("tlsVerify", event.target.checked)} disabled={busy} />
                <span>校验 Origin TLS 证书</span>
              </label>
              <div className="field-group">
                <label htmlFor="tls-server-name">TLS Server Name（可选）</label>
                <input id="tls-server-name" value={values.tlsServerName} onChange={(event) => field("tlsServerName", event.target.value)} disabled={busy} />
              </div>
            </div>
          ) : null}
          </section>
        <section className="form-section">
          <h3>健康检查</h3>
          <div className="segmented-control" role="group" aria-label="健康检查类型">
            <button type="button" className={values.healthType === "none" ? "active" : ""} onClick={() => field("healthType", "none")} aria-pressed={values.healthType === "none"} disabled={busy}>关闭</button>
            <button type="button" className={values.healthType === "TCP" ? "active" : ""} onClick={() => field("healthType", "TCP")} aria-pressed={values.healthType === "TCP"} disabled={busy}>TCP</button>
            <button type="button" className={values.healthType === "HTTP" ? "active" : ""} onClick={() => field("healthType", "HTTP")} aria-pressed={values.healthType === "HTTP"} disabled={busy || values.originScheme === "tcp"}>HTTP</button>
          </div>
          {values.healthType === "HTTP" ? (
            <div className="field-group compact-field">
              <label htmlFor="health-path">探测 Path</label>
              <input id="health-path" value={values.healthPath} onChange={(event) => field("healthPath", event.target.value)} pattern="/.*" required disabled={busy} />
            </div>
          ) : null}
          <p className="form-hint">未展示的 Proxy 参数保持不变；切换健康检查类型时使用 Server 冻结默认值。</p>
        </section>

        </details>

        <footer className="dialog-actions sticky-actions">
          <button className="secondary-button" type="button" onClick={onClose} disabled={busy}>取消</button>
          <button className="primary-button" type="submit" disabled={busy || !values.name.trim() || !values.originHost.trim()}>
            {busy ? <LoaderCircle className="button-spinner" aria-hidden="true" /> : null}
            {editing ? "保存 Service" : "创建 Service"}
          </button>
        </footer>
      </form>
    </FlowPage>
  );
}

export function ManagementView({ csrfToken, onSessionExpired }: {
  csrfToken: string;
  onSessionExpired: (message?: string) => void;
}) {
  const [tunnels, setTunnels] = useState<Tunnel[]>([]);
  const [tunnelPageToken, setTunnelPageToken] = useState<string>();
  const [selectedTunnelID, setSelectedTunnelID] = useState<string>();
  const [detailTab, setDetailTab] = useState<"overview" | "services">("overview");
  const [tunnelSearch, setTunnelSearch] = useState("");
  const [connectorSearch, setConnectorSearch] = useState("");
  const [loadingTunnels, setLoadingTunnels] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [workspace, setWorkspace] = useState<WorkspaceState>({ status: "idle" });
  const [dialog, setDialog] = useState<DialogState>();
  const [busy, setBusy] = useState(false);
  const [feedback, setFeedback] = useState<Feedback>();
  const selectedTunnelIDRef = useRef(selectedTunnelID);
  const workspaceRequestGenerationRef = useRef(0);
  selectedTunnelIDRef.current = selectedTunnelID;

  function selectTunnel(tunnelID?: string) {
    workspaceRequestGenerationRef.current += 1;
    selectedTunnelIDRef.current = tunnelID;
    setSelectedTunnelID(tunnelID);
    setDetailTab("overview");
    setConnectorSearch("");
    setWorkspace(tunnelID ? { status: "loading" } : { status: "idle" });
  }

  function closeDialog() {
    if (!busy) setDialog(undefined);
  }

  function handleUnauthorized(status: number) {
    if (status === 401) {
      onSessionExpired("管理会话已过期，请重新登录。");
      return true;
    }
    return false;
  }

  async function loadTunnels(pageToken?: string, append = false, signal?: AbortSignal) {
    append ? setLoadingMore(true) : setLoadingTunnels(true);
    try {
      const result = await apiClient.GET("/tunnels", {
        signal,
        params: { query: { page_size: PAGE_SIZE, ...(pageToken ? { page_token: pageToken } : {}) } },
      });
      if (result.data) {
        const items = result.data.items as ReadonlyArray<Tunnel>;
        setTunnels((current) => append ? [...current, ...items] : [...items]);
        setTunnelPageToken(result.data.next_page_token);

        return;
      }
      if (!handleUnauthorized(result.response.status)) {
        setFeedback({ tone: "error", message: errorMessage(result.error, "无法读取 Tunnel 列表。") });
      }
    } catch (error: unknown) {
      if (!(error instanceof DOMException && error.name === "AbortError")) {
        setFeedback({ tone: "error", message: "无法连接管理服务，请稍后重试。" });
      }
    } finally {
      append ? setLoadingMore(false) : setLoadingTunnels(false);
    }
  }

  async function loadWorkspace(tunnelID: string, signal?: AbortSignal) {
    const generation = ++workspaceRequestGenerationRef.current;
    setWorkspace({ status: "loading" });
    try {
      // 列表不携带单项 ETag；详情、Connector 和 Service 并行读取，写操作只使用 Server 返回的原始 ETag。
      const [tunnelResult, connectorResult, serviceResult] = await Promise.all([
        apiClient.GET("/tunnels/{tunnel_id}", { signal, params: { path: { tunnel_id: tunnelID } } }),
        apiClient.GET("/tunnels/{tunnel_id}/connectors", {
          signal,
          params: { path: { tunnel_id: tunnelID }, query: { page_size: PAGE_SIZE } },
        }),
        apiClient.GET("/services", {
          signal,
          params: { query: { tunnel_id: tunnelID, page_size: PAGE_SIZE } },
        }),
      ]);
      for (const result of [tunnelResult, connectorResult, serviceResult]) {
        if (handleUnauthorized(result.response.status)) {
          return;
        }
      }
      if (!tunnelResult.data || !connectorResult.data || !serviceResult.data) {
        const error = tunnelResult.error ?? connectorResult.error ?? serviceResult.error;
        if (generation === workspaceRequestGenerationRef.current && selectedTunnelIDRef.current === tunnelID) {
          setWorkspace({ status: "error", message: errorMessage(error, "无法读取 Tunnel 工作区。") });
        }
        return;
      }
      const etag = tunnelResult.response.headers.get("ETag");
      if (!etag) {
        if (generation === workspaceRequestGenerationRef.current && selectedTunnelIDRef.current === tunnelID) {
          setWorkspace({ status: "error", message: "Server 未返回 Tunnel ETag，已阻止可能丢失更新的操作。" });
        }
        return;
      }
      if (generation !== workspaceRequestGenerationRef.current || selectedTunnelIDRef.current !== tunnelID) return;
      setWorkspace({
        status: "ready",
        tunnel: tunnelResult.data,
        tunnelETag: etag,
        connectors: [...(connectorResult.data.items as ReadonlyArray<Connector>)],
        connectorPageToken: connectorResult.data.next_page_token,
        services: [...(serviceResult.data.items as ReadonlyArray<Service>)],
        servicePageToken: serviceResult.data.next_page_token,
      });
    } catch (error: unknown) {
      if (!(error instanceof DOMException && error.name === "AbortError")
        && generation === workspaceRequestGenerationRef.current
        && selectedTunnelIDRef.current === tunnelID) {
        setWorkspace({ status: "error", message: "无法连接管理服务，请稍后重试。" });
      }
    }
  }

  useEffect(() => {
    const controller = new AbortController();
    void loadTunnels(undefined, false, controller.signal);
    return () => controller.abort();
  }, []);

  useEffect(() => {
    if (!selectedTunnelID) {
      setWorkspace({ status: "idle" });
      return;
    }
    const controller = new AbortController();
    void loadWorkspace(selectedTunnelID, controller.signal);
    return () => controller.abort();
  }, [selectedTunnelID]);

  async function refreshSelected() {
    await Promise.all([
      loadTunnels(),
      selectedTunnelID ? loadWorkspace(selectedTunnelID) : Promise.resolve(),
    ]);
  }

  async function createTunnel(name: string) {
    setBusy(true);
    try {
      const result = await apiClient.POST("/tunnels", {
        params: { header: { Origin: window.location.origin } },
        headers: { "X-XTunnel-CSRF": csrfToken },
        body: { name },
      });
      if (result.data) {
        selectTunnel(result.data.tunnel.id);
        // 成功响应立即切换步骤，后续列表刷新不会回到可再次提交的名称表单。
        // Secret 仅保留在当前指引状态，退出后释放，不进入 URL、Storage 或日志。
        setDialog({ kind: "install-tunnel", tunnel: result.data.tunnel, credential: result.data.credential as ConnectionCredential });
        await loadTunnels();
        return;
      }
      if (!handleUnauthorized(result.response.status)) {
        setFeedback({ tone: "error", message: errorMessage(result.error, "创建 Tunnel 失败。") });
      }
    } catch {
      setFeedback({ tone: "error", message: "无法连接管理服务，创建 Tunnel 失败。" });
    } finally {
      setBusy(false);
    }
  }

  async function addServiceAfterInstallation(tunnel: Tunnel) {
    setBusy(true);
    try {
      const result = await apiClient.GET("/tunnels/{tunnel_id}", { params: { path: { tunnel_id: tunnel.id } } });
      if (handleUnauthorized(result.response.status)) return;
      const etag = result.response.headers.get("ETag");
      if (!result.data || !etag) {
        setFeedback({ tone: "error", message: errorMessage(result.error, "无法读取 Tunnel 最新版本，请重试。") });
        return;
      }
      setDetailTab("services");
      setDialog({ kind: "create-service", tunnel: result.data, etag });
    } catch {
      setFeedback({ tone: "error", message: "无法连接管理服务，请重试。" });
    } finally {
      setBusy(false);
    }
  }

  async function renameTunnel(dialogValue: Extract<DialogState, { kind: "rename-tunnel" }>, name: string) {
    setBusy(true);
    try {
      const result = await apiClient.PATCH("/tunnels/{tunnel_id}", {
        params: { path: { tunnel_id: dialogValue.tunnel.id }, header: { Origin: window.location.origin, "If-Match": dialogValue.etag } },
        headers: { "X-XTunnel-CSRF": csrfToken, "Content-Type": "application/merge-patch+json" },
        body: { name },
      });
      if (result.data) {
        setDialog(undefined);
        setFeedback({ tone: "success", message: "Tunnel 名称已更新。" });
        await refreshSelected();
        return;
      }
      if (!handleUnauthorized(result.response.status)) {
        if (result.response.status === 412) {
          setDialog(undefined);
          setFeedback({ tone: "error", message: "Tunnel 已被其他操作更新，工作区已刷新，请重新打开重命名。" });
          await refreshSelected();
        } else {
          setFeedback({ tone: "error", message: errorMessage(result.error, "更新 Tunnel 失败。") });
        }
      }
    } catch {
      setFeedback({ tone: "error", message: "无法连接管理服务，Tunnel 名称未更新。" });
    } finally {
      setBusy(false);
    }
  }

  // 请求期间入口会 disabled 并失焦，因此保留点击时的按钮供抽屉关闭后恢复焦点。
  async function revealCredential(tunnel: Tunnel, returnFocus: HTMLElement) {
    setBusy(true);
    try {
      const result = await apiClient.GET("/tunnels/{tunnel_id}/token", { params: { path: { tunnel_id: tunnel.id } } });
      if (result.data) {
        if (selectedTunnelIDRef.current !== tunnel.id) return;
        setDialog({ kind: "credential", credential: result.data as ConnectionCredential, title: "安装连接器", returnFocus });
        return;
      }
      if (!handleUnauthorized(result.response.status)) {
        setFeedback({ tone: "error", message: errorMessage(result.error, "无法读取当前 ACTIVE Token。") });
      }
    } catch {
      setFeedback({ tone: "error", message: "无法连接管理服务，未读取 Token。" });
    } finally {
      setBusy(false);
    }
  }

  async function mutateTunnel(action: "rotate-token" | "revoke-token" | "revoke" | "delete") {
    if (workspace.status !== "ready") return;
    setBusy(true);
    const tunnel = workspace.tunnel;
    const common = {
      params: { path: { tunnel_id: tunnel.id }, header: { Origin: window.location.origin, "If-Match": workspace.tunnelETag } },
      headers: { "X-XTunnel-CSRF": csrfToken },
    } as const;
    try {
      const result = action === "rotate-token"
        ? await apiClient.POST("/tunnels/{tunnel_id}/token/rotate", common)
        : action === "revoke-token"
          ? await apiClient.POST("/tunnels/{tunnel_id}/token/revoke", common)
          : action === "revoke"
            ? await apiClient.POST("/tunnels/{tunnel_id}/revoke", common)
            : await apiClient.DELETE("/tunnels/{tunnel_id}", common);
      if (result.response.status === 401) {
        handleUnauthorized(401);
        return;
      }
      if (action === "delete" && result.response.status === 204) {
        setDialog(undefined);
        selectTunnel(undefined);
        setWorkspace({ status: "idle" });
        setFeedback({ tone: "success", message: `Tunnel“${tunnel.name}”已删除。` });
        await loadTunnels();
        return;
      }
      if (result.data) {
        if (action === "rotate-token" && "connection_token" in result.data) {
          setDialog({ kind: "credential", credential: result.data as ConnectionCredential, title: "安装连接器 · Token 已轮换" });
        } else {
          setDialog(undefined);
        }
        const messages = {
          "rotate-token": "Token 已轮换；现有 Session 保持在线，请部署新 Token。",
          "revoke-token": "当前 Token 已撤销新认证。",
          revoke: "Tunnel 及其运行时访问已撤销。",
          delete: "Tunnel 已删除。",
        };
        setFeedback({ tone: "success", message: messages[action] });
        await refreshSelected();
        return;
      }
      setFeedback({
        tone: "error",
        message: result.response.status === 412
          ? "Tunnel 已被其他操作更新，工作区已刷新，请重新确认。"
          : errorMessage(result.error, "操作失败。"),
      });
      if (result.response.status === 412) {
        setDialog(undefined);
        await refreshSelected();
      }
    } catch {
      setFeedback({ tone: "error", message: "无法连接管理服务，Tunnel 操作未完成。" });
    } finally {
      setBusy(false);
    }
  }

  async function getServiceForMutation(serviceID: string) {
    try {
      // Service ETag 同时绑定 Service 与父 Tunnel 版本，因此每次写操作前重新读取，绝不从 version 推导。
      const result = await apiClient.GET("/services/{service_id}", { params: { path: { service_id: serviceID } } });
      if (handleUnauthorized(result.response.status)) return;
      const etag = result.response.headers.get("ETag");
      if (!result.data || !etag) {
        setFeedback({ tone: "error", message: errorMessage(result.error, "无法读取最新 Service 或 ETag。") });
        return;
      }
      return { service: result.data, etag };
    } catch {
      setFeedback({ tone: "error", message: "无法连接管理服务，未读取最新 Service。" });
      return;
    }
  }

  async function openEditService(serviceID: string) {
    setBusy(true);
    try {
      const current = await getServiceForMutation(serviceID);
      if (current && selectedTunnelIDRef.current === current.service.tunnel_id) {
        setDialog({ kind: "edit-service", ...current });
      }
    } catch {
      setFeedback({ tone: "error", message: "无法连接管理服务，Service 未保存。" });
    } finally {
      setBusy(false);
    }
  }

  async function saveService(dialogValue: Extract<DialogState, { kind: "create-service" | "edit-service" }>, body: CreateServiceRequest | UpdateServiceRequest) {
    setBusy(true);
    try {
      const result = dialogValue.kind === "create-service"
        ? await apiClient.POST("/services", {
          params: { header: { Origin: window.location.origin, "If-Match": dialogValue.etag } },
          headers: { "X-XTunnel-CSRF": csrfToken },
          body: body as CreateServiceRequest,
        })
        : await apiClient.PATCH("/services/{service_id}", {
          params: { path: { service_id: dialogValue.service.id }, header: { Origin: window.location.origin, "If-Match": dialogValue.etag } },
          headers: { "X-XTunnel-CSRF": csrfToken, "Content-Type": "application/merge-patch+json" },
          body: body as UpdateServiceRequest,
        });
      if (result.data) {
        setDialog(undefined);
        setFeedback({ tone: "success", message: dialogValue.kind === "create-service" ? "Service 已创建。" : "Service 已更新。" });
        await refreshSelected();
        return;
      }
      if (!handleUnauthorized(result.response.status)) {
        if (result.response.status === 412) {
          setDialog(undefined);
          setFeedback({ tone: "error", message: "父 Tunnel 或 Service 已被其他操作更新，工作区已刷新，请重新打开 Service 表单。" });
          await refreshSelected();
        } else {
          setFeedback({ tone: "error", message: errorMessage(result.error, "保存 Service 失败。") });
        }
      }
    } catch {
      setFeedback({ tone: "error", message: "无法连接管理服务，Service 操作未完成。" });
    } finally {
      setBusy(false);
    }
  }

  async function openServiceConfirmation(serviceID: string, intent: "toggle" | "delete") {
    setBusy(true);
    try {
      const current = await getServiceForMutation(serviceID);
      if (!current) return;
      if (selectedTunnelIDRef.current !== current.service.tunnel_id) return;
      const action = intent === "delete" ? "delete" : current.service.enabled ? "disable" : "enable";
      const title = action === "delete" ? "删除 Service" : action === "disable" ? "禁用 Service" : "启用 Service";
      const message = action === "delete"
        ? `删除“${current.service.name}”会在同一事务内移除其 Public Exposure，此操作不可撤销。`
        : action === "disable"
          ? `禁用“${current.service.name}”后将停止新流量，但保留配置。`
          : `启用“${current.service.name}”并发布新的 Tunnel Revision？`;
      setDialog({
        kind: "confirm",
        title,
        message,
        actionLabel: action === "delete" ? "删除 Service" : action === "disable" ? "确认禁用" : "确认启用",
        danger: action === "delete",
        run: async () => mutateService(current.service, action, current.etag),
      });
    } finally {
      setBusy(false);
    }
  }

  async function mutateService(service: Service, action: "enable" | "disable" | "delete", etag: string) {
    setBusy(true);
    try {
      const common = {
        params: { path: { service_id: service.id }, header: { Origin: window.location.origin, "If-Match": etag } },
        headers: { "X-XTunnel-CSRF": csrfToken },
      } as const;
      const result = action === "enable"
        ? await apiClient.POST("/services/{service_id}/enable", common)
        : action === "disable"
          ? await apiClient.POST("/services/{service_id}/disable", common)
          : await apiClient.DELETE("/services/{service_id}", common);
      if (result.response.status === 401) {
        handleUnauthorized(401);
        return;
      }
      if ((action === "delete" && result.response.status === 204) || result.data) {
        setDialog(undefined);
        setFeedback({ tone: "success", message: action === "delete" ? "Service 已删除。" : action === "enable" ? "Service 已启用。" : "Service 已禁用。" });
        await refreshSelected();
        return;
      }
      setFeedback({ tone: "error", message: result.response.status === 412 ? "Service 已被其他操作更新，工作区已刷新，请重新确认。" : errorMessage(result.error, "操作 Service 失败。") });
      if (result.response.status === 412) {
        setDialog(undefined);
        await refreshSelected();
      }
    } catch {
      setFeedback({ tone: "error", message: "无法连接管理服务，Service 操作未完成。" });
    } finally {
      setBusy(false);
    }
  }

  async function loadMoreWorkspace(kind: "connectors" | "services") {
    if (workspace.status !== "ready") return;
    const token = kind === "connectors" ? workspace.connectorPageToken : workspace.servicePageToken;
    if (!token) return;
    setLoadingMore(true);
    const tunnelID = workspace.tunnel.id;
    try {
      if (kind === "connectors") {
        const result = await apiClient.GET("/tunnels/{tunnel_id}/connectors", {
          params: { path: { tunnel_id: tunnelID }, query: { page_size: PAGE_SIZE, page_token: token } },
        });
        if (result.data) {
          setWorkspace((current) => current.status === "ready" && current.tunnel.id === tunnelID
            ? { ...current, connectors: [...current.connectors, ...(result.data.items as ReadonlyArray<Connector>)], connectorPageToken: result.data.next_page_token }
            : current);
        } else if (selectedTunnelIDRef.current === tunnelID && !handleUnauthorized(result.response.status)) {
          setFeedback({ tone: "error", message: errorMessage(result.error, "无法读取更多 Connector。") });
        }
      } else {
        const result = await apiClient.GET("/services", {
          params: { query: { tunnel_id: tunnelID, page_size: PAGE_SIZE, page_token: token } },
        });
        if (result.data) {
          setWorkspace((current) => current.status === "ready" && current.tunnel.id === tunnelID
            ? { ...current, services: [...current.services, ...(result.data.items as ReadonlyArray<Service>)], servicePageToken: result.data.next_page_token }
            : current);
        } else if (selectedTunnelIDRef.current === tunnelID && !handleUnauthorized(result.response.status)) {
          setFeedback({ tone: "error", message: errorMessage(result.error, "无法读取更多 Service。") });
        }
      }
    } catch {
      if (selectedTunnelIDRef.current === tunnelID) {
        setFeedback({ tone: "error", message: `无法连接管理服务，未读取更多 ${kind === "connectors" ? "Connector" : "Service"}。` });
      }
    } finally {
      setLoadingMore(false);
    }
  }

  const fullPage = dialog?.kind === "create-tunnel" || dialog?.kind === "install-tunnel" || dialog?.kind === "create-service" || dialog?.kind === "edit-service";
  const selectedTunnel = workspace.status === "ready" ? workspace.tunnel : tunnels.find((item) => item.id === selectedTunnelID);
  const matchingTunnels = tunnels.filter((tunnel) =>
    `${tunnel.name} ${tunnel.id} ${statusLabel(tunnel.status)}`.toLocaleLowerCase().includes(tunnelSearch.trim().toLocaleLowerCase()),
  );
  const matchingConnectors = workspace.status === "ready" ? workspace.connectors.filter((connector) =>
    `${connector.id} ${connector.hostname} ${connector.os} ${connector.arch} ${connector.version} ${statusLabel(connector.status)}`
      .toLocaleLowerCase().includes(connectorSearch.trim().toLocaleLowerCase()),
  ) : [];

  return (
    <div className="management-page">
      {feedback ? (
        <div className={`management-feedback ${feedback.tone}`} role={feedback.tone === "error" ? "alert" : "status"}>
          {feedback.tone === "success" ? <Check aria-hidden="true" /> : <AlertTriangle aria-hidden="true" />}
          <span>{feedback.message}</span>
          <button type="button" onClick={() => setFeedback(undefined)} aria-label="关闭提示"><X aria-hidden="true" /></button>
        </div>
      ) : null}

      {!fullPage && (!selectedTunnelID ? (
        <>
          <div className="management-toolbar">
            <div><p className="breadcrumb">工作台 / 服务与隧道</p><h1>服务与隧道</h1><p>将设备连接到隧道，集中管理服务与访问入口。</p></div>
            <button className="primary-button" type="button" onClick={() => setDialog({ kind: "create-tunnel" })}><CirclePlus aria-hidden="true" />创建 Tunnel</button>
          </div>
          <section className="tunnel-directory detail-card" aria-label="隧道管理">
            <header className="detail-card-heading">
              <div><h2>隧道 <span className="item-count">{tunnels.length} 已加载</span></h2><p>打开隧道，查看连接器状态并管理服务。</p></div>
              <button className="secondary-button" type="button" onClick={() => void loadTunnels()} disabled={loadingTunnels} aria-label="刷新 Tunnel 列表"><RefreshCw aria-hidden="true" />刷新</button>
            </header>
            <div className="table-toolbar"><label className="table-search"><Search aria-hidden="true" /><input type="search" aria-label="搜索隧道" placeholder="搜索隧道名称、ID 或状态" value={tunnelSearch} onChange={(event) => setTunnelSearch(event.target.value)} /></label><span>搜索当前已加载的隧道</span></div>
            {loadingTunnels ? <div className="table-loading" role="status"><LoaderCircle className="button-spinner" aria-hidden="true" />正在读取隧道…</div> : null}
            <div className="resource-table-scroll" tabIndex={0} role="region" aria-label="隧道表格滚动区域">
              <table className="resource-table" aria-label="隧道列表">
                <thead><tr><th>隧道名称</th><th>状态</th><th>在线连接器</th><th>服务</th><th>创建时间</th><th aria-label="操作" /></tr></thead>
                <tbody>
                  {matchingTunnels.map((tunnel) => <tr key={tunnel.id}>
                    <td><button className="table-name-button" type="button" aria-label={`打开隧道 ${tunnel.name}`} onClick={() => selectTunnel(tunnel.id)}><Cable aria-hidden="true" /><strong>{tunnel.name}</strong></button><code className="table-secondary">{tunnel.id}</code></td>
                    <td><StatusBadge status={tunnel.status} /></td><td>{tunnel.connectors_online}</td><td>{tunnel.services_count}</td><td>{formatDate(tunnel.created_at)}</td>
                    <td><ChevronRight className="table-chevron" aria-hidden="true" /></td>
                  </tr>)}
                  {!loadingTunnels && matchingTunnels.length === 0 ? <tr><td colSpan={6}><div className="table-empty"><Cable aria-hidden="true" /><strong>{tunnels.length === 0 ? "还没有隧道" : "没有匹配的隧道"}</strong><p>{tunnels.length === 0 ? "创建第一条隧道，获取连接器安装指引。" : "尝试其他关键词，或继续加载更多隧道。"}</p></div></td></tr> : null}
                </tbody>
              </table>
            </div>
            <footer className="table-footer"><span>显示 {matchingTunnels.length} 条 · 已加载 {tunnels.length} 条</span>{tunnelPageToken ? <button className="load-more-button" type="button" onClick={() => void loadTunnels(tunnelPageToken, true)} disabled={loadingMore}>加载更多隧道</button> : null}</footer>
          </section>
        </>
      ) : (
        <div className="tunnel-detail">
          <nav className="detail-breadcrumb" aria-label="隧道导航"><button type="button" onClick={() => selectTunnel(undefined)} disabled={busy} aria-label="返回隧道列表"><ArrowLeft aria-hidden="true" />隧道</button><ChevronRight aria-hidden="true" /><span>{selectedTunnel?.name ?? "详情"}</span></nav>
          {workspace.status === "loading" ? <div className="canvas-loading" role="status" aria-live="polite"><LoaderCircle className="button-spinner" aria-hidden="true" />正在读取隧道详情…</div> : null}
          {workspace.status === "error" ? <div className="dashboard-error" role="alert"><AlertTriangle aria-hidden="true" /><div><strong>工作区暂不可用</strong><p>{workspace.message}</p></div><button type="button" onClick={() => void loadWorkspace(selectedTunnelID)}><RefreshCw aria-hidden="true" />重试</button></div> : null}
          {workspace.status === "ready" ? <>
            <header className="detail-heading">
              <div><div className="tunnel-title-line"><h1>{workspace.tunnel.name}</h1><StatusBadge status={workspace.tunnel.status} /></div><p>管理隧道连接、服务与访问凭据。</p></div>
              <div className="tunnel-heading-actions">
                <button className="secondary-button" type="button" onClick={() => setDialog({ kind: "rename-tunnel", tunnel: workspace.tunnel, etag: workspace.tunnelETag })} disabled={busy}><Pencil aria-hidden="true" />重命名</button>
                <button className="secondary-button" type="button" onClick={() => void loadWorkspace(workspace.tunnel.id)} disabled={busy}><RefreshCw aria-hidden="true" />刷新</button>
                <button className="primary-button" type="button" onClick={(event) => void revealCredential(workspace.tunnel, event.currentTarget)} disabled={busy || workspace.tunnel.status === "REVOKED"}><KeyRound aria-hidden="true" />安装连接器</button>
              </div>
            </header>
            <div className="detail-tabs" role="tablist" aria-label="隧道详情" onKeyDown={(event) => {
              if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
              event.preventDefault();
              const next = event.key === "Home" ? "overview" : event.key === "End" ? "services" : detailTab === "overview" ? "services" : "overview";
              setDetailTab(next);
              event.currentTarget.querySelector<HTMLButtonElement>(`#tunnel-tab-${next}`)?.focus();
            }}>
              <button id="tunnel-tab-overview" role="tab" type="button" aria-selected={detailTab === "overview"} aria-controls="tunnel-panel-overview" tabIndex={detailTab === "overview" ? 0 : -1} onClick={() => setDetailTab("overview")}>概览</button>
              <button id="tunnel-tab-services" role="tab" type="button" aria-selected={detailTab === "services"} aria-controls="tunnel-panel-services" tabIndex={detailTab === "services" ? 0 : -1} onClick={() => setDetailTab("services")}>服务 <span>{workspace.tunnel.services_count}</span></button>
            </div>
            {detailTab === "overview" ? <div id="tunnel-panel-overview" role="tabpanel" aria-labelledby="tunnel-tab-overview" className="detail-panel">
              <section className="detail-card" aria-labelledby="tunnel-basic-heading">
                <header className="detail-card-heading"><h2 id="tunnel-basic-heading">基本信息</h2></header>
                <dl className="tunnel-basic-grid">
                  <div><dt>隧道名称</dt><dd>{workspace.tunnel.name}</dd></div>
                  <div><dt>隧道 ID</dt><dd><code>{workspace.tunnel.id}</code></dd></div>
                  <div><dt>状态</dt><dd><StatusBadge status={workspace.tunnel.status} /></dd></div>
                  <div><dt>创建时间</dt><dd>{formatDate(workspace.tunnel.created_at)}</dd></div>
                  <div><dt>最近在线</dt><dd>{formatDate(workspace.tunnel.last_seen_at)}</dd></div>
                  <div><dt>期望配置版本</dt><dd>{workspace.tunnel.desired_revision}</dd></div>
                  <div><dt>在线连接器</dt><dd>{workspace.tunnel.connectors_online}</dd></div>
                  <div><dt>服务数量</dt><dd>{workspace.tunnel.services_count}</dd></div>
                  <div><dt>活动连接</dt><dd>{workspace.tunnel.active_connections}</dd></div>
                </dl>
              </section>
              <section className="detail-card connector-section" aria-labelledby="connector-list-heading">
                <header className="detail-card-heading"><div><h2 id="connector-list-heading">连接器 <span className="item-count">{workspace.connectors.length} 已加载</span></h2><p>查看接入这条隧道的设备与实时连接状态。</p></div></header>
                <div className="table-toolbar"><label className="table-search"><Search aria-hidden="true" /><input type="search" aria-label="搜索连接器" placeholder="搜索连接器 ID、主机名、平台或状态" value={connectorSearch} onChange={(event) => setConnectorSearch(event.target.value)} /></label><span>搜索当前已加载的连接器</span></div>
                <div className="resource-table-scroll" tabIndex={0} role="region" aria-label="连接器表格滚动区域">
                  <table className="resource-table connector-table" aria-label="连接器列表">
                    <thead><tr><th>主机名 / 连接器 ID</th><th>平台</th><th>版本</th><th>状态</th><th>配置</th><th>连接</th><th>最近心跳</th></tr></thead>
                    <tbody>
                      {matchingConnectors.map((connector) => <tr key={connector.id}>
                        <td><strong>{connector.hostname}</strong><code className="table-secondary">{connector.id}</code></td><td>{connector.os}/{connector.arch}</td><td>{connector.version}</td><td><StatusBadge status={connector.status} /></td>
                        <td><span className={connector.config_ready ? "configuration-ready" : "configuration-pending"}>{connector.config_ready ? "已确认" : "等待确认"}</span><small className="table-secondary">版本 {connector.observed_revision}</small></td>
                        <td>{connector.active_connections} 活动<small className="table-secondary">{connector.idle_work_connections} 空闲</small></td><td>{formatDate(connector.last_heartbeat_at)}</td>
                      </tr>)}
                      {matchingConnectors.length === 0 ? <tr><td colSpan={7}><div className="table-empty"><Cable aria-hidden="true" /><strong>{workspace.connectors.length === 0 ? "尚无连接器接入" : "没有匹配的连接器"}</strong><p>{workspace.connectors.length === 0 ? "点击上方“安装连接器”，在设备上运行 Agent。" : "尝试其他关键词，或继续加载更多连接器。"}</p></div></td></tr> : null}
                    </tbody>
                  </table>
                </div>
                <footer className="table-footer"><span>显示 {matchingConnectors.length} 条 · 已加载 {workspace.connectors.length} 条</span>{workspace.connectorPageToken ? <button className="load-more-button" type="button" onClick={() => void loadMoreWorkspace("connectors")} disabled={loadingMore}>加载更多 Connector</button> : null}</footer>
              </section>


            </div> : <div id="tunnel-panel-services" role="tabpanel" aria-labelledby="tunnel-tab-services" className="detail-panel">
              <section className="workbench-section service-section">
                <div className="workbench-section-heading">
                  <div><h2>服务</h2><p>管理这条隧道的源站、访问入口与健康检查。</p></div>
                  <button className="primary-button" type="button" onClick={() => setDialog({ kind: "create-service", tunnel: workspace.tunnel, etag: workspace.tunnelETag })} disabled={busy || workspace.tunnel.status === "REVOKED"}><CirclePlus aria-hidden="true" />创建 Service</button>
                </div>
                <div className="service-list">
                  {workspace.services.length === 0 ? <div className="section-empty"><ServerCog aria-hidden="true" /><span>还没有服务。添加一个源站，为这条隧道配置访问入口。</span></div> : null}
                  {workspace.services.map((service) => (
                    <article className="service-row" key={service.id}>
                      <div className="service-identity">
                        <span className={`service-protocol protocol-${service.origin.scheme}`}>{service.origin.scheme.toUpperCase()}</span>
                        <div><strong>{service.name}</strong><code>{service.id}</code></div>
                      </div>
                      <div className="service-route">
                        <span>ORIGIN</span>
                        <strong>{service.origin.scheme}://{service.origin.host}:{service.origin.port}</strong>
                        <small>{service.exposure?.type === "http" ? `${service.exposure.hostname}${service.exposure.path_prefix}` : service.exposure?.type === "tcp" ? `TCP :${service.exposure.public_port}` : "无公网 Exposure"}</small>
                      </div>
                      <div className="service-runtime">
                        <StatusBadge status={service.status} />
                        <small>{service.healthy_connectors} healthy · {service.active_connections} active</small>
                      </div>
                      <div className="service-actions">
                        <button className="icon-button" type="button" onClick={() => void openEditService(service.id)} aria-label={`编辑 ${service.name}`} disabled={busy}><Pencil aria-hidden="true" /></button>
                        <button className="icon-button" type="button" onClick={() => void openServiceConfirmation(service.id, "toggle")} aria-label={service.enabled ? `禁用 ${service.name}` : `启用 ${service.name}`} disabled={busy}>{service.enabled ? <PowerOff aria-hidden="true" /> : <Power aria-hidden="true" />}</button>
                        <button className="icon-button danger" type="button" onClick={() => void openServiceConfirmation(service.id, "delete")} aria-label={`删除 ${service.name}`} disabled={busy}><Trash2 aria-hidden="true" /></button>
                      </div>
                    </article>
                  ))}
                </div>
                {workspace.servicePageToken ? <button className="load-more-button inline" type="button" onClick={() => void loadMoreWorkspace("services")} disabled={loadingMore}>加载更多 Service</button> : null}
              </section>


            </div>}
              <section className="danger-zone">
                <div><span>SECURITY BOUNDARY</span><h3>凭据与 Tunnel 生命周期</h3><p>轮换只阻止旧 Token 新认证；撤销 Tunnel 会关闭全部运行时访问。删除前必须先移除所有 Service。</p></div>
                <div>
                  <button type="button" onClick={() => setDialog({ kind: "confirm", title: "轮换 Connection Token", message: "轮换会签发新版本，现有 Session 保持在线；旧 Token 无法建立新 Session。", actionLabel: "轮换并显示新 Token", run: async () => mutateTunnel("rotate-token") })} disabled={busy || workspace.tunnel.status === "REVOKED"}><RotateCw aria-hidden="true" />轮换 Token</button>
                  <button type="button" onClick={() => setDialog({ kind: "confirm", title: "撤销当前 Token", message: "撤销后当前 Token 不能建立新 Session；现有 Session 不被强制关闭。", actionLabel: "撤销 Token", danger: true, run: async () => mutateTunnel("revoke-token") })} disabled={busy || workspace.tunnel.status === "REVOKED"}><ShieldOff aria-hidden="true" />撤销 Token</button>
                  <button type="button" onClick={() => setDialog({ kind: "confirm", title: "撤销 Tunnel", message: "这会撤销全部 Token，并关闭该 Tunnel 的 Session、Idle WorkConn 与 ActiveWork。", actionLabel: "撤销 Tunnel", danger: true, run: async () => mutateTunnel("revoke") })} disabled={busy || workspace.tunnel.status === "REVOKED"}><PowerOff aria-hidden="true" />撤销 Tunnel</button>
                  <button className="danger" type="button" onClick={() => setDialog({ kind: "confirm", title: "删除 Tunnel", message: "只有没有 Service 引用的 Tunnel 才能删除。删除不会级联 Service 或 Route。", actionLabel: "永久删除", danger: true, run: async () => mutateTunnel("delete") })} disabled={busy}><Trash2 aria-hidden="true" />删除 Tunnel</button>
                </div>
              </section>
          </> : null}
        </div>
      ))}
      {dialog?.kind === "install-tunnel" ? <TunnelInstallation tunnel={dialog.tunnel} credential={dialog.credential} busy={busy} onClose={closeDialog} onAddService={() => void addServiceAfterInstallation(dialog.tunnel)} onSessionExpired={onSessionExpired} /> : null}
      {dialog?.kind === "create-tunnel" ? <TunnelNameDialog mode="create" busy={busy} onClose={closeDialog} onSubmit={createTunnel} /> : null}
      {dialog?.kind === "rename-tunnel" ? <TunnelNameDialog mode="rename" initialName={dialog.tunnel.name} busy={busy} onClose={closeDialog} onSubmit={(name) => renameTunnel(dialog, name)} /> : null}
      {dialog?.kind === "credential" ? <CredentialDialog credential={dialog.credential} title={dialog.title} onClose={closeDialog} returnFocus={dialog.returnFocus} /> : null}
      {dialog && (dialog.kind === "create-service" || dialog.kind === "edit-service") ? <ServiceDialog dialog={dialog} busy={busy} onClose={closeDialog} onSubmit={(body) => saveService(dialog, body)} /> : null}
      {dialog?.kind === "confirm" ? <ConfirmDialog dialog={dialog} busy={busy} onClose={closeDialog} /> : null}
    </div>
  );
}
