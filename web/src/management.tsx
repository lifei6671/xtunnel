import {
  AlertTriangle,
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
  | { kind: "rename-tunnel"; tunnel: Tunnel; etag: string }
  | { kind: "create-service"; tunnel: Tunnel; etag: string }
  | { kind: "edit-service"; service: Service; etag: string }
  | { kind: "credential"; credential: ConnectionCredential; title: string }
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

function Dialog({ title, eyebrow, children, onClose }: {
  title: string;
  eyebrow: string;
  children: ReactNode;
  onClose: () => void;
}) {
  const dialogRef = useRef<HTMLElement>(null);
  const onCloseRef = useRef(onClose);
  const previousFocusRef = useRef<HTMLElement | null>(
    document.activeElement instanceof HTMLElement ? document.activeElement : null,
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
    <div className="dialog-backdrop" role="presentation" onMouseDown={(event) => {
      if (event.target === event.currentTarget) {
        onClose();
      }
    }}>
      <section ref={dialogRef} className="management-dialog" role="dialog" aria-modal="true" aria-labelledby="management-dialog-title" tabIndex={-1}>
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

function TunnelNameDialog({ mode, initialName = "", busy, onClose, onSubmit }: {
  mode: "create" | "rename";
  initialName?: string;
  busy: boolean;
  onClose: () => void;
  onSubmit: (name: string) => Promise<void>;
}) {
  const nameId = useId();
  const [name, setName] = useState(initialName);

  return (
    <Dialog
      title={mode === "create" ? "创建 Tunnel" : "重命名 Tunnel"}
      eyebrow={mode === "create" ? "NEW CONTROL CHAIN" : "TUNNEL IDENTITY"}
      onClose={onClose}
    >
      <form onSubmit={(event) => {
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
            {mode === "create" ? "创建并显示部署指引" : "保存名称"}
          </button>
        </footer>
      </form>
    </Dialog>
  );
}

function CredentialDialog({ credential, title, onClose }: {
  credential: ConnectionCredential;
  title: string;
  onClose: () => void;
}) {
  const [copied, setCopied] = useState<string>();

  async function copy(label: string, value: string) {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(label);
      window.setTimeout(() => setCopied((current) => current === label ? undefined : current), 1600);
    } catch {
      setCopied(`error:${label}`);
    }
  }

  return (
    <Dialog title={title} eyebrow="ONE-TIME SECRET VIEW" onClose={onClose}>
      <div className="secret-notice">
        <KeyRound aria-hidden="true" />
        <p>完整 Token 仅保存在当前弹层内存中。复制到目标环境后立即关闭，本页不会持久化。</p>
      </div>
      <div className="credential-token">
        <div>
          <span>Connection Token · v{credential.token_version}</span>
          <code>{credential.connection_token}</code>
        </div>
        <button type="button" onClick={() => void copy("token", credential.connection_token)}>
          {copied === "token" ? <Check aria-hidden="true" /> : <Clipboard aria-hidden="true" />}
          {copied === "token" ? "已复制" : copied === "error:token" ? "复制失败" : "复制"}
        </button>
      </div>
      <div className="deployment-list">
        {credential.deployment_commands.map((item) => (
          <article key={item.environment}>
            <div>
              <strong>{item.environment.replaceAll("_", " ")}</strong>
              <code>{item.command}</code>
            </div>
            <button type="button" onClick={() => void copy(item.environment, item.command)}>
              {copied === item.environment ? <Check aria-hidden="true" /> : <Clipboard aria-hidden="true" />}
              {copied === item.environment ? "已复制" : copied === `error:${item.environment}` ? "复制失败" : "复制命令"}
            </button>
          </article>
        ))}
      </div>
      <footer className="dialog-actions">
        <button className="primary-button" type="button" onClick={onClose}>完成并清除页面 Secret</button>
      </footer>
    </Dialog>
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
    <Dialog title={editing ? "编辑 Service" : "创建 Service"} eyebrow="SERVICE ROUTE" onClose={onClose}>
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
          <h3>Origin</h3>
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
          <h3>Public Exposure</h3>
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
          <h3>Health Check</h3>
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

        <footer className="dialog-actions sticky-actions">
          <button className="secondary-button" type="button" onClick={onClose} disabled={busy}>取消</button>
          <button className="primary-button" type="submit" disabled={busy || !values.name.trim() || !values.originHost.trim()}>
            {busy ? <LoaderCircle className="button-spinner" aria-hidden="true" /> : null}
            {editing ? "保存 Service" : "创建 Service"}
          </button>
        </footer>
      </form>
    </Dialog>
  );
}

export function ManagementView({ csrfToken, onSessionExpired }: {
  csrfToken: string;
  onSessionExpired: (message?: string) => void;
}) {
  const [tunnels, setTunnels] = useState<Tunnel[]>([]);
  const [tunnelPageToken, setTunnelPageToken] = useState<string>();
  const [selectedTunnelID, setSelectedTunnelID] = useState<string>();
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
        if (!append && !selectedTunnelIDRef.current && items[0]) selectTunnel(items[0].id);
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
        await loadTunnels();
        setFeedback({ tone: "success", message: `Tunnel“${result.data.tunnel.name}”已创建。` });
        // Secret 只进入一次性弹层状态，关闭弹层或退出页面后即由 React 释放，不进入 URL、Storage 或日志。
        setDialog({ kind: "credential", credential: result.data.credential as ConnectionCredential, title: "部署第一个 Connector" });
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

  async function revealCredential(tunnel: Tunnel) {
    setBusy(true);
    try {
      const result = await apiClient.GET("/tunnels/{tunnel_id}/token", { params: { path: { tunnel_id: tunnel.id } } });
      if (result.data) {
        if (selectedTunnelIDRef.current !== tunnel.id) return;
        setDialog({ kind: "credential", credential: result.data as ConnectionCredential, title: "添加 Connector" });
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
          setDialog({ kind: "credential", credential: result.data as ConnectionCredential, title: "Token 已轮换" });
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

  const selectedTunnel = workspace.status === "ready" ? workspace.tunnel : tunnels.find((item) => item.id === selectedTunnelID);

  return (
    <div className="management-page">
      {feedback ? (
        <div className={`management-feedback ${feedback.tone}`} role={feedback.tone === "error" ? "alert" : "status"}>
          {feedback.tone === "success" ? <Check aria-hidden="true" /> : <AlertTriangle aria-hidden="true" />}
          <span>{feedback.message}</span>
          <button type="button" onClick={() => setFeedback(undefined)} aria-label="关闭提示"><X aria-hidden="true" /></button>
        </div>
      ) : null}

      <div className="management-toolbar">
        <div>
          <p className="breadcrumb">工作台 / 服务与隧道</p>
          <h1>链路工作台</h1>
          <p>选择 Tunnel 后，按部署 Connector、确认上线、配置 Service 的顺序完成日常管理。</p>
        </div>
        <button className="primary-button" type="button" onClick={() => setDialog({ kind: "create-tunnel" })}>
          <CirclePlus aria-hidden="true" />创建 Tunnel
        </button>
      </div>

      <div className="tunnel-workbench">
        <aside className="tunnel-rail" aria-label="Tunnel 列表">
          <header>
            <div><span>TUNNELS</span><strong>{tunnels.length}</strong></div>
            <button className="icon-button" type="button" onClick={() => void loadTunnels()} aria-label="刷新 Tunnel 列表"><RefreshCw aria-hidden="true" /></button>
          </header>
          {loadingTunnels ? <div className="rail-state" role="status" aria-live="polite"><LoaderCircle className="button-spinner" aria-hidden="true" />读取中…</div> : null}
          {!loadingTunnels && tunnels.length === 0 ? (
            <div className="rail-empty"><Cable aria-hidden="true" /><strong>还没有 Tunnel</strong><span>创建后会在这里形成一条控制链。</span></div>
          ) : null}
          <div className="tunnel-list">
            {tunnels.map((tunnel) => (
              <button
                type="button"
                className={tunnel.id === selectedTunnelID ? "active" : ""}
                onClick={() => selectTunnel(tunnel.id)}
                aria-pressed={tunnel.id === selectedTunnelID}
                key={tunnel.id}
              >
                <span className={`tunnel-signal status-${tunnel.status.toLowerCase()}`} aria-hidden="true" />
                <span><strong>{tunnel.name}</strong><small>{tunnel.connectors_online} Connector · {tunnel.services_count} Service</small></span>
                <ChevronRight aria-hidden="true" />
              </button>
            ))}
          </div>
          {tunnelPageToken ? (
            <button className="load-more-button" type="button" onClick={() => void loadTunnels(tunnelPageToken, true)} disabled={loadingMore}>
              {loadingMore ? <LoaderCircle className="button-spinner" aria-hidden="true" /> : null}加载更多
            </button>
          ) : null}
        </aside>

        <section className="tunnel-canvas">
          {!selectedTunnel ? (
            <div className="canvas-empty"><ServerCog aria-hidden="true" /><h2>从创建一条 Tunnel 开始</h2><p>Tunnel 是 Token、Connector 与 Service 的唯一管理边界。</p></div>
          ) : null}
          {selectedTunnel && workspace.status === "loading" ? <div className="canvas-loading" role="status" aria-live="polite"><LoaderCircle className="button-spinner" aria-hidden="true" />正在装配 Tunnel 工作区…</div> : null}
          {workspace.status === "error" ? (
            <div className="dashboard-error" role="alert"><AlertTriangle aria-hidden="true" /><div><strong>工作区暂不可用</strong><p>{workspace.message}</p></div><button type="button" onClick={() => selectedTunnelID && void loadWorkspace(selectedTunnelID)}><RefreshCw aria-hidden="true" />重试</button></div>
          ) : null}
          {workspace.status === "ready" ? (
            <>
              <header className="tunnel-heading">
                <div>
                  <div className="tunnel-title-line"><h2>{workspace.tunnel.name}</h2><StatusBadge status={workspace.tunnel.status} /></div>
                  <code>{workspace.tunnel.id}</code>
                </div>
                <div className="tunnel-heading-actions">
                  <button className="secondary-button" type="button" onClick={() => setDialog({ kind: "rename-tunnel", tunnel: workspace.tunnel, etag: workspace.tunnelETag })}><Pencil aria-hidden="true" />重命名</button>
                  <button className="secondary-button" type="button" onClick={() => void loadWorkspace(workspace.tunnel.id)}><RefreshCw aria-hidden="true" />刷新</button>
                </div>
              </header>

              <section className="chain-summary" aria-label="Tunnel 摘要">
                <div><span>Connector</span><strong>{workspace.tunnel.connectors_online}</strong><small>当前在线</small></div>
                <div><span>Service</span><strong>{workspace.tunnel.services_count}</strong><small>持久配置</small></div>
                <div><span>Active</span><strong>{workspace.tunnel.active_connections}</strong><small>实时连接</small></div>
                <div><span>Revision</span><strong>{workspace.tunnel.desired_revision}</strong><small>期望配置</small></div>
                <div><span>Last Seen</span><strong className="date-value">{formatDate(workspace.tunnel.last_seen_at)}</strong><small>Server 权威时间</small></div>
              </section>

              <section className="workbench-section deployment-section">
                <div className="workbench-section-heading">
                  <div><span>01 · DEPLOY</span><h3>部署 Connector</h3><p>“添加 Connector”读取同一枚 ACTIVE Token，不创建数据库行或新版本。</p></div>
                  <button className="primary-button" type="button" onClick={() => void revealCredential(workspace.tunnel)} disabled={busy || workspace.tunnel.status === "REVOKED"}><KeyRound aria-hidden="true" />添加 Connector</button>
                </div>
                <div className="connector-grid">
                  {workspace.connectors.length === 0 ? <div className="section-empty"><Cable aria-hidden="true" /><span>尚无在线 Connector。复制部署命令并启动 Agent 后，此处会出现实时 Session。</span></div> : null}
                  {workspace.connectors.map((connector) => (
                    <article className="connector-card" key={connector.id}>
                      <header><div><strong>{connector.hostname}</strong><code>{connector.id}</code></div><StatusBadge status={connector.status} /></header>
                      <dl>
                        <div><dt>平台</dt><dd>{connector.os}/{connector.arch}</dd></div>
                        <div><dt>版本</dt><dd>{connector.version}</dd></div>
                        <div><dt>Revision</dt><dd>{connector.observed_revision}</dd></div>
                        <div><dt>连接</dt><dd>{connector.active_connections} active · {connector.idle_work_connections} idle</dd></div>
                      </dl>
                      <footer><span className={connector.config_ready ? "ready-dot" : "warning-dot"} />{connector.config_ready ? "配置已确认" : "等待配置确认"}<time>{formatDate(connector.last_heartbeat_at)}</time></footer>
                    </article>
                  ))}
                </div>
                {workspace.connectorPageToken ? <button className="load-more-button inline" type="button" onClick={() => void loadMoreWorkspace("connectors")} disabled={loadingMore}>加载更多 Connector</button> : null}
              </section>

              <section className="workbench-section service-section">
                <div className="workbench-section-heading">
                  <div><span>02 · ROUTE</span><h3>配置 Service</h3><p>Origin、Public Exposure 与健康检查通过 Server 持久化并下发，无需修改 Agent 本地配置。</p></div>
                  <button className="primary-button" type="button" onClick={() => setDialog({ kind: "create-service", tunnel: workspace.tunnel, etag: workspace.tunnelETag })} disabled={busy || workspace.tunnel.status === "REVOKED"}><CirclePlus aria-hidden="true" />创建 Service</button>
                </div>
                <div className="service-list">
                  {workspace.services.length === 0 ? <div className="section-empty"><ServerCog aria-hidden="true" /><span>还没有 Service。创建后 Server 会推进 Tunnel Revision 并下发完整快照。</span></div> : null}
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

              <section className="danger-zone">
                <div><span>SECURITY BOUNDARY</span><h3>凭据与 Tunnel 生命周期</h3><p>轮换只阻止旧 Token 新认证；撤销 Tunnel 会关闭全部运行时访问。删除前必须先移除所有 Service。</p></div>
                <div>
                  <button type="button" onClick={() => setDialog({ kind: "confirm", title: "轮换 Connection Token", message: "轮换会签发新版本，现有 Session 保持在线；旧 Token 无法建立新 Session。", actionLabel: "轮换并显示新 Token", run: async () => mutateTunnel("rotate-token") })} disabled={busy || workspace.tunnel.status === "REVOKED"}><RotateCw aria-hidden="true" />轮换 Token</button>
                  <button type="button" onClick={() => setDialog({ kind: "confirm", title: "撤销当前 Token", message: "撤销后当前 Token 不能建立新 Session；现有 Session 不被强制关闭。", actionLabel: "撤销 Token", danger: true, run: async () => mutateTunnel("revoke-token") })} disabled={busy || workspace.tunnel.status === "REVOKED"}><ShieldOff aria-hidden="true" />撤销 Token</button>
                  <button type="button" onClick={() => setDialog({ kind: "confirm", title: "撤销 Tunnel", message: "这会撤销全部 Token，并关闭该 Tunnel 的 Session、Idle WorkConn 与 ActiveWork。", actionLabel: "撤销 Tunnel", danger: true, run: async () => mutateTunnel("revoke") })} disabled={busy || workspace.tunnel.status === "REVOKED"}><PowerOff aria-hidden="true" />撤销 Tunnel</button>
                  <button className="danger" type="button" onClick={() => setDialog({ kind: "confirm", title: "删除 Tunnel", message: "只有没有 Service 引用的 Tunnel 才能删除。删除不会级联 Service 或 Route。", actionLabel: "永久删除", danger: true, run: async () => mutateTunnel("delete") })} disabled={busy}><Trash2 aria-hidden="true" />删除 Tunnel</button>
                </div>
              </section>
            </>
          ) : null}
        </section>
      </div>

      {dialog?.kind === "create-tunnel" ? <TunnelNameDialog mode="create" busy={busy} onClose={closeDialog} onSubmit={createTunnel} /> : null}
      {dialog?.kind === "rename-tunnel" ? <TunnelNameDialog mode="rename" initialName={dialog.tunnel.name} busy={busy} onClose={closeDialog} onSubmit={(name) => renameTunnel(dialog, name)} /> : null}
      {dialog?.kind === "credential" ? <CredentialDialog credential={dialog.credential} title={dialog.title} onClose={closeDialog} /> : null}
      {dialog && (dialog.kind === "create-service" || dialog.kind === "edit-service") ? <ServiceDialog dialog={dialog} busy={busy} onClose={closeDialog} onSubmit={(body) => saveService(dialog, body)} /> : null}
      {dialog?.kind === "confirm" ? <ConfirmDialog dialog={dialog} busy={busy} onClose={closeDialog} /> : null}
    </div>
  );
}
