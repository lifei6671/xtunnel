import { Download, Filter, LoaderCircle, RefreshCw, ShieldAlert } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";
import type { FormEvent } from "react";

import { apiClient } from "./api/client";
import type { components, operations } from "./api/schema.gen";

type APIErrorEnvelope = components["schemas"]["ErrorResponse"];
type AuditEvent = components["schemas"]["SecurityAuditEvent"];
type AuditList = components["schemas"]["SecurityAuditEventList"];
type AuditQuery = NonNullable<operations["listSecurityAuditEvents"]["parameters"]["query"]>;

type AuditState =
  | { status: "loading" }
  | { status: "ready"; page: AuditList }
  | { status: "error"; message: string };

type Filters = {
  action: string;
  result: string;
  resourceType: string;
  resourceID: string;
  occurredFrom: string;
  occurredTo: string;
};

const emptyFilters: Filters = {
  action: "",
  result: "",
  resourceType: "",
  resourceID: "",
  occurredFrom: "",
  occurredTo: "",
};

const actionLabels = {
  GATEWAY_KEY_ROTATE: "Gateway 身份轮换",
  CONNECTION_TOKEN_REVEAL: "查看连接 Token",
  CONNECTION_TOKEN_ROTATE: "轮换连接 Token",
  CONNECTION_TOKEN_REVOKE: "撤销连接 Token",
  TUNNEL_REVOKE: "撤销 Tunnel",
} as const satisfies Readonly<Record<AuditEvent["action"], string>>;

function apiMessage(error: APIErrorEnvelope | undefined) {
  return error?.error.message;
}

function utcDateTime(value: string) {
  return value ? new Date(value).toISOString() : undefined;
}

function auditQuery(filters: Filters, pageToken?: string): AuditQuery {
  return {
    page_size: 50,
    page_token: pageToken,
    action: (filters.action || undefined) as AuditQuery["action"],
    result: (filters.result || undefined) as AuditQuery["result"],
    resource_type: (filters.resourceType || undefined) as AuditQuery["resource_type"],
    resource_id: filters.resourceID || undefined,
    occurred_from: utcDateTime(filters.occurredFrom),
    occurred_to: utcDateTime(filters.occurredTo),
  };
}

function exportURL(filters: Filters) {
  const { page_size: _pageSize, page_token: _pageToken, ...query } = auditQuery(filters);
  const parameters = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined) {
      parameters.set(key, String(value));
    }
  }
  const suffix = parameters.toString();
  return `/api/v1/security-audit-events/export${suffix ? `?${suffix}` : ""}`;
}

export function SecurityAuditView({ onSessionExpired }: {
  onSessionExpired: (message?: string) => void;
}) {
  const resourceID = useId();
  const fromID = useId();
  const toID = useId();
  const [draft, setDraft] = useState<Filters>(emptyFilters);
  const [filters, setFilters] = useState<Filters>(emptyFilters);
  const [state, setState] = useState<AuditState>({ status: "loading" });
  const [downloading, setDownloading] = useState(false);
  const requestSequence = useRef(0);
  const listController = useRef<AbortController | undefined>(undefined);
  const downloadController = useRef<AbortController | undefined>(undefined);

  async function load(selected: Filters, pageToken?: string) {
    listController.current?.abort();
    const controller = new AbortController();
    listController.current = controller;
    const sequence = ++requestSequence.current;
    setState({ status: "loading" });
    try {
      const result = await apiClient.GET("/security-audit-events", {
        params: { query: auditQuery(selected, pageToken) },
        signal: controller.signal,
      });
      if (sequence !== requestSequence.current) {
        return;
      }
      if (result.data) {
        setState({ status: "ready", page: result.data as AuditList });
        return;
      }
      if (result.response.status === 401) {
        onSessionExpired("管理会话已过期，请重新登录。");
        return;
      }
      setState({
        status: "error",
        message: apiMessage(result.error as APIErrorEnvelope | undefined) ?? "无法读取安全审计事件。",
      });
    } catch (error) {
      if (!(error instanceof DOMException && error.name === "AbortError") && sequence === requestSequence.current) {
        setState({ status: "error", message: "无法连接管理服务，请稍后重试。" });
      }
    }
  }

  useEffect(() => {
    void load(filters);
    return () => {
      requestSequence.current += 1;
      listController.current?.abort();
      downloadController.current?.abort();
    };
  }, []);

  function applyFilters(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setFilters(draft);
    void load(draft);
  }

  async function download() {
    downloadController.current?.abort();
    const controller = new AbortController();
    downloadController.current = controller;
    setDownloading(true);
    let objectURL: string | undefined;
    try {
      const response = await fetch(exportURL(filters), {
        credentials: "same-origin",
        headers: { Accept: "application/x-ndjson" },
        signal: controller.signal,
      });
      if (response.status === 401) {
        onSessionExpired("管理会话已过期，请重新登录。");
        return;
      }
      if (!response.ok) {
        setState({ status: "error", message: "安全审计导出失败，请稍后重试。" });
        return;
      }
      const blob = await response.blob();
      objectURL = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = objectURL;
      anchor.download = "xtunnel-security-audit.ndjson";
      document.body.append(anchor);
      anchor.click();
      anchor.remove();
    } catch (error) {
      if (!(error instanceof DOMException && error.name === "AbortError")) {
        setState({ status: "error", message: "安全审计导出中断，未生成完整文件。" });
      }
    } finally {
      if (objectURL) {
        URL.revokeObjectURL(objectURL);
      }
      if (downloadController.current === controller) {
        downloadController.current = undefined;
        setDownloading(false);
      }
    }
  }

  return (
    <div className="audit-stack">
      <div className="page-heading">
        <p className="breadcrumb">工作台 / 安全审计</p>
        <h1>安全审计</h1>
        <p>查询和导出都只读取已提交的 append-only 事件；筛选条件由 Server 权威执行。</p>
      </div>

      <form className="audit-filters" onSubmit={applyFilters}>
        <label>动作
          <select value={draft.action} onChange={(event) => setDraft({ ...draft, action: event.target.value })}>
            <option value="">全部</option>
            {Object.entries(actionLabels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}
          </select>
        </label>
        <label>结果
          <select value={draft.result} onChange={(event) => setDraft({ ...draft, result: event.target.value })}>
            <option value="">全部</option><option value="SUCCEEDED">成功</option><option value="FAILED">失败</option>
          </select>
        </label>
        <label>资源类型
          <select value={draft.resourceType} onChange={(event) => setDraft({ ...draft, resourceType: event.target.value })}>
            <option value="">全部</option><option value="GATEWAY_IDENTITY">Gateway 身份</option>
            <option value="TUNNEL_TOKEN">Tunnel Token</option><option value="TUNNEL">Tunnel</option>
          </select>
        </label>
        <label htmlFor={resourceID}>资源 ID
          <input id={resourceID} value={draft.resourceID} maxLength={256} onChange={(event) => setDraft({ ...draft, resourceID: event.target.value })} />
        </label>
        <label htmlFor={fromID}>开始时间
          <input id={fromID} type="datetime-local" value={draft.occurredFrom} onChange={(event) => setDraft({ ...draft, occurredFrom: event.target.value })} />
        </label>
        <label htmlFor={toID}>结束时间
          <input id={toID} type="datetime-local" value={draft.occurredTo} onChange={(event) => setDraft({ ...draft, occurredTo: event.target.value })} />
        </label>
        <div className="audit-actions">
          <button type="submit"><Filter aria-hidden="true" />应用筛选</button>
          <button type="button" onClick={() => void download()} disabled={downloading}>
            {downloading ? <LoaderCircle className="button-spinner" aria-hidden="true" /> : <Download aria-hidden="true" />}
            {downloading ? "导出中" : "导出 NDJSON"}
          </button>
        </div>
      </form>

      {state.status === "loading" ? <section className="dashboard-loading" aria-live="polite" aria-busy="true"><LoaderCircle className="button-spinner" aria-hidden="true" />正在读取审计事件…</section> : null}
      {state.status === "error" ? <section className="dashboard-error" role="alert"><ShieldAlert aria-hidden="true" /><div><strong>安全审计暂不可用</strong><p>{state.message}</p></div><button type="button" onClick={() => void load(filters)}><RefreshCw aria-hidden="true" />重试</button></section> : null}
      {state.status === "ready" ? (
        <section className="audit-results" aria-live="polite">
          {state.page.items.length === 0 ? <p className="audit-empty">当前筛选范围没有审计事件。</p> : (
            <div className="audit-table-wrap"><table><thead><tr><th>时间</th><th>动作</th><th>结果</th><th>资源</th><th>错误码</th><th>关联</th></tr></thead>
              <tbody>{state.page.items.map((event) => <tr key={event.event_id}>
                <td><time dateTime={event.occurred_at}>{new Date(event.occurred_at).toLocaleString("zh-CN", { hour12: false })}</time></td>
                <td>{actionLabels[event.action]}</td><td><span className={`audit-result ${event.result.toLowerCase()}`}>{event.result}</span></td>
                <td><strong>{event.resource_type}</strong><code>{event.resource_id}</code></td>
                <td><code>{event.error_code ?? "—"}</code></td><td><code>{event.request_id ?? event.trace_id ?? "—"}</code></td>
              </tr>)}</tbody></table></div>
          )}
          <div className="audit-pagination">
            <button type="button" disabled={!state.page.next_page_token} onClick={() => state.page.next_page_token && void load(filters, state.page.next_page_token)}>下一页</button>
          </div>
        </section>
      ) : null}
    </div>
  );
}
