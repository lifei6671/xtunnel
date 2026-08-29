async page => {
  await page.unroute("**/api/v1/**");
  await page.route("**/api/v1/**", async route => {
    const request = route.request();
    const path = request.url().replace(/^https?:\/\/[^/]+/, "").split("?")[0];
    if (request.method() === "GET" && path === "/api/v1/auth/me") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          admin: { id: "adm_01J00000000000000000000000", username: "operator" },
          csrf_token: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
          expires_at: "2026-08-30T12:00:00Z",
        }),
      });
      return;
    }
    if (request.method() === "GET" && path === "/api/v1/dashboard") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          server_status: "DEGRADED",
          counts: {
            tunnels_total: 12,
            tunnels_online: 9,
            tunnels_offline: 3,
            connectors_online: 17,
            services_total: 28,
            services_ready: 23,
            services_error: 2,
            active_connections: 146,
          },
          traffic: {
            availability: "UNAVAILABLE",
            connections_today: null,
            ingress_bytes_today: null,
            egress_bytes_today: null,
          },
          recent_errors: { availability: "UNAVAILABLE", items: [] },
          generated_at: "2026-08-29T12:08:00Z",
        }),
      });
      return;
    }
    await route.fulfill({ status: 404, body: "not found" });
  });
  await page.reload();
  await page.waitForTimeout(500);
}
