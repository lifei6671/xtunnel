import {
  Bot,
  LayoutDashboard,
  Network,
  RadioTower,
  Settings,
} from "lucide-react";

const navigation = [
  { label: "概览", icon: LayoutDashboard },
  { label: "Agent 管理", icon: Bot },
  { label: "服务与隧道", icon: Network },
  { label: "访问入口", icon: RadioTower },
  { label: "系统设置", icon: Settings },
] as const;

// M0-08 只展示已经验证的工程状态，真实业务数据等待 M5 API 接入。
const foundationStatus = [
  ["HTTPS 开发", "已启用", "本地开发仅接受受信任的 Loopback 证书"],
  ["同源代理", "已就绪", "/api/v1 保持浏览器 Host 与 Origin"],
  ["生产构建", "已嵌入", "静态资源随 Server Binary 一起交付"],
] as const;

export function App() {
  return (
    <div className="app-shell">
      <header className="topbar">
        <div className="brand" aria-label="XTunnel">
          <span className="brand-mark" aria-hidden="true">
            XT
          </span>
          <span className="brand-name">XTunnel</span>
        </div>
        <div className="topbar-context">管理控制台</div>
        <div className="topbar-version">Standalone · V0.1</div>
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
                  {index !== 0 && <span className="nav-pending">待接入</span>}
                </div>
              );
            })}
          </div>
        </nav>

        <div className="sidebar-footer">
          <span className="status-dot" aria-hidden="true" />
          Web 基础链路就绪
        </div>
      </aside>

      <main className="main-content" id="overview">
        <div className="page-heading">
          <p className="breadcrumb">工作台 / 概览</p>
          <h1>概览</h1>
          <p>查看 XTunnel 管理面的接入状态。业务功能将在 REST API 与认证完成后开放。</p>
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
                      <span>当前仅完成管理界面工程骨架，不展示推测数据。</span>
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
