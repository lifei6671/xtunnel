import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 90_000,
  expect: { timeout: 10_000 },
  forbidOnly: Boolean(process.env.CI),
  reporter: "line",
  outputDir: process.env.XTUNNEL_E2E_OUTPUT_DIR,
  preserveOutput: "never",
  use: {
    ...devices["Desktop Chrome"],
    baseURL: "https://127.0.0.1:5173",
    ignoreHTTPSErrors: true,
    screenshot: "off",
    trace: "off",
    video: "off",
  },
});
