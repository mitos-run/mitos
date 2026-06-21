// The claim-mode orchestration: drives a minimal mitos cluster client through
// the Paperclip provider lifecycle (acquire -> assert installs -> teardown with
// extract-then-delete). The MitosClaimClient interface is the thin seam this
// logic talks to, so it is unit testable with a fake (no cluster, no Paperclip
// SDK). The real implementation wraps @mitos/sdk AgentRun cluster mode
// (sdk/typescript/src/client.ts), whose makeTerminator already patches outputs
// before deleting the claim.

import type { OutputSpec, SandboxClaim } from "./claim-mapping.js";
import { assertAdapterInstalls, terminateToOutputs } from "./claim-mapping.js";
import type { InstalledProbe } from "./claim-mapping.js";

/**
 * The minimal cluster surface claim mode needs. Mirrors a subset of the
 * @mitos/sdk AgentRun / K8sApi verbs.
 */
export interface MitosClaimClient {
  /** Create a SandboxClaim and wait until it is Ready; returns its endpoint. */
  createClaim(claim: SandboxClaim): Promise<{ endpoint: string }>;
  /**
   * Probe the running sandbox for the adapter binaries on PATH. Used by the
   * claim-time install assertion. Implemented over the sandbox exec API.
   */
  probeInstalls(claimName: string, required: string[]): Promise<InstalledProbe>;
  /**
   * Patch a claim's spec.outputs (the terminate-with-outputs directives) so the
   * controller dehydrates the workspace into a committed revision on teardown.
   */
  patchOutputs(claimName: string, outputs: OutputSpec[]): Promise<void>;
  /** Delete the claim (teardown). */
  deleteClaim(claimName: string): Promise<void>;
}

/** Raised when the claim-time adapter-install assertion fails. */
export class AdapterInstallError extends Error {
  readonly code = "adapter_install_missing";
  readonly missing: string[];
  constructor(missing: string[]) {
    super(
      `required adapter binaries are not present in the pool snapshot: ${missing.join(", ")}. ` +
        "Adapter installs are baked at pool build, not per run; rebuild the pool's template with these adapters.",
    );
    this.name = "AdapterInstallError";
    this.missing = missing;
  }
}

/**
 * Acquire a lease: create the claim, then assert the required adapter binaries
 * are present (never install them). On a missing adapter the claim is torn down
 * and an actionable error is thrown, so a non-conforming pool fails fast rather
 * than running a half-provisioned sandbox.
 */
export async function acquireWithAssertion(
  client: MitosClaimClient,
  claim: SandboxClaim,
  requiredAdapters: string[],
): Promise<{ endpoint: string }> {
  const ready = await client.createClaim(claim);
  if (requiredAdapters.length > 0) {
    const probe = await client.probeInstalls(claim.metadata.name, requiredAdapters);
    const assertion = assertAdapterInstalls(requiredAdapters, probe);
    if (!assertion.ok) {
      // Tear down the non-conforming sandbox; no artifacts to extract.
      await client.deleteClaim(claim.metadata.name).catch(() => undefined);
      throw new AdapterInstallError(assertion.missing);
    }
  }
  return ready;
}

/**
 * Teardown: extract workspace artifacts FIRST, then delete the claim. The
 * ordering is the contract: patchOutputs must complete (the controller
 * dehydrates the workspace into a committed revision on the way out) before
 * deleteClaim runs. A delete-before-extract would lose the artifacts, so this
 * function never reorders the two calls and never deletes when the output patch
 * throws.
 */
export async function teardownWithExtract(
  client: MitosClaimClient,
  claimName: string,
  extractPaths: string[],
): Promise<void> {
  const outputs = terminateToOutputs(extractPaths);
  // Extract first: patch the terminate-with-outputs directives onto the claim.
  await client.patchOutputs(claimName, outputs);
  // Only after the artifacts are queued for dehydration do we delete.
  await client.deleteClaim(claimName);
}
