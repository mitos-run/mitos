# frozen_string_literal: true

require_relative "lib/mitos/version"

Gem::Specification.new do |spec|
  spec.name = "mitos"
  spec.version = Mitos::VERSION
  spec.authors = ["mitos"]
  spec.summary = "Ruby SDK for mitos snapshot-fork sandboxes (direct sandbox-server mode)."
  spec.description =
    "A thin, dependency-free Ruby client for the standalone and hosted mitos " \
    "sandbox-server REST API: create templates, fork sandboxes, run exec, and " \
    "terminate. Mirrors the Python and TypeScript direct-mode SDKs. Kubernetes " \
    "cluster mode is served by the Python and TypeScript SDKs only."
  spec.homepage = "https://mitos.run"
  spec.license = "Apache-2.0"

  spec.required_ruby_version = ">= 2.6.0"

  spec.files = Dir["lib/**/*.rb"] + ["README.md"]
  spec.require_paths = ["lib"]

  # No runtime dependencies: the SDK uses only the Ruby standard library
  # (net/http, json, uri, securerandom).
  spec.metadata = {
    "homepage_uri" => "https://mitos.run",
    "documentation_uri" => "https://mitos.run/docs",
    "source_code_uri" => "https://github.com/mitos-run/mitos/tree/main/sdk/ruby",
    "bug_tracker_uri" => "https://github.com/mitos-run/mitos/issues"
  }
end
