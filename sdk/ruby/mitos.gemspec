# frozen_string_literal: true

require_relative "lib/mitos/version"

Gem::Specification.new do |spec|
  spec.name = "mitos"
  spec.version = Mitos::VERSION
  spec.authors = ["mitos"]
  spec.summary = "Ruby SDK for mitos snapshot-fork sandboxes (direct and Kubernetes cluster mode)."
  spec.description =
    "A thin, dependency-free Ruby client for mitos. Direct mode talks to the " \
    "standalone and hosted sandbox-server REST API (create templates, fork " \
    "sandboxes, run exec, terminate). Cluster mode (Mitos::AgentRun) drives the " \
    "Kubernetes mitos.run CRDs (SandboxPool, Sandbox, Workspace) over the " \
    "Kubernetes REST API with no Kubernetes client gem. Mirrors the Python and " \
    "TypeScript SDKs."
  spec.homepage = "https://mitos.run"
  spec.license = "Apache-2.0"

  spec.required_ruby_version = ">= 2.6.0"

  spec.files = Dir["lib/**/*.rb"] + ["README.md"]
  spec.require_paths = ["lib"]

  # No runtime dependencies: the SDK uses only the Ruby standard library.
  # Direct mode uses net/http, json, uri, securerandom; cluster mode adds
  # openssl (TLS / CA), yaml (kubeconfig), and base64 (in-cluster and Secret
  # token decoding), all stdlib.
  spec.metadata = {
    "homepage_uri" => "https://mitos.run",
    "documentation_uri" => "https://mitos.run/docs",
    "source_code_uri" => "https://github.com/mitos-run/mitos/tree/main/sdk/ruby",
    "bug_tracker_uri" => "https://github.com/mitos-run/mitos/issues"
  }
end
