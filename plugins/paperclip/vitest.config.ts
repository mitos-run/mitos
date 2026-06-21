import { defineConfig } from "vitest/config";

// Tests cover the pure claim-mapping logic and the claim-mode orchestration
// (with a fake client). These modules have no dependency on the external
// @paperclipai/plugin-sdk (a workspace package resolved only in the Paperclip
// monorepo), so the mapping contract is verifiable in this repo on its own.
export default defineConfig({
  test: {
    environment: "node",
    include: ["test/**/*.test.ts"],
  },
});
