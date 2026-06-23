import type { PaperclipPluginManifestV1 } from "@paperclipai/plugin-sdk";

const manifest: PaperclipPluginManifestV1 = {
  id: "paperclip.sandbox-provider",
  apiVersion: 1,
  version: "0.1.0",
  displayName: "Sandbox Provider (Firecracker)",
  description:
    "Self-hosted sandbox provider using mitos-run/mitos. Sub-millisecond Firecracker microVM forking with per-volume fork policies.",
  author: "Paperclip",
  categories: ["automation"],
  capabilities: ["environment.drivers.register"],
  entrypoints: {
    worker: "./dist/worker.js",
  },
  environmentDrivers: [
    {
      driverKey: "sandbox",
      kind: "sandbox_provider",
      displayName: "Sandbox (Firecracker)",
      description:
        "Self-hosted sandboxes via mitos-run/mitos. Firecracker microVMs with CoW forking (~0.8ms), volume fork policies, and k8s-native management.",
      configSchema: {
        type: "object",
        properties: {
          backend: {
            type: "string",
            enum: ["server", "claim"],
            description:
              "Execution backend. server: standalone sandbox-server REST API. claim: a mitos Sandbox on Kubernetes (mitos.run/v1), with lease lifetime mapped to sandbox lifetime.ttl/lifetime.idleTimeout, callback-bridge mapped to a sandbox-time egress allow, and secrets injected at sandbox create time. Production enablement of claim mode is gated on mitos #3 (fork-correctness) and #163 (failure/GC) being green in CI.",
            default: "server",
          },
          serverUrl: {
            type: "string",
            description:
              "server backend only: URL of the sandbox-server or forkd HTTP API. Example: http://sandbox-server:8080",
          },
          template: {
            type: "string",
            description:
              "server backend only: template ID to fork sandboxes from. Must be pre-created on the server.",
            default: "default",
          },
          pool: {
            type: "string",
            description:
              "claim backend only: the SandboxPool to claim from. Adapter installs are baked into this pool at build time and asserted at claim time, never installed per run.",
          },
          namespace: {
            type: "string",
            description: "claim backend only: Kubernetes namespace for the Sandbox.",
            default: "default",
          },
          maxLifetimeMin: {
            type: "number",
            description:
              "claim backend only: lease max lifetime in minutes; maps to the claim spec.timeout (wall-clock cap). Omit for no limit.",
          },
          idleTimeoutMin: {
            type: "number",
            description:
              "claim backend only: idle-reap window in minutes; maps to the claim spec.idleTimeout. Omit for no idle limit.",
          },
          bridgeEndpoint: {
            type: "string",
            description:
              "claim backend only: the instance callback-bridge endpoint (host:port or URL). Becomes the sole egress allow entry over an otherwise default-deny posture.",
          },
          requiredAdapters: {
            type: "array",
            items: { type: "string" },
            description:
              "claim backend only: adapter binaries that must be present in the pool snapshot. Asserted at claim time by a PATH probe; never installed per run.",
          },
          extractPaths: {
            type: "array",
            items: { type: "string" },
            description:
              "claim backend only: /workspace subtrees to extract into a committed WorkspaceRevision on teardown, before the claim is deleted. Empty captures the whole workspace.",
          },
          timeoutMs: {
            type: "number",
            description: "Timeout for sandbox operations in milliseconds.",
            default: 30000,
          },
          reuseLease: {
            type: "boolean",
            description:
              "server backend only: keep the sandbox alive across runs instead of terminating on release.",
            default: false,
          },
        },
      },
    },
  ],
};

export default manifest;
