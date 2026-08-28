import createClient from "openapi-fetch";

import type { paths } from "./schema.gen";

// Management API 使用同源 Session Cookie；路径和请求/响应类型只来自 OpenAPI 生成契约。
export const apiClient = createClient<paths>({
  baseUrl: "/api/v1",
  credentials: "same-origin",
});
