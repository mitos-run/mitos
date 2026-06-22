# frozen_string_literal: true

require "mitos/version"
require "mitos/errors"
require "mitos/sandbox"
require "mitos/sandbox_server"

# Mitos is the official Ruby SDK for mitos snapshot-fork sandboxes.
#
# This SDK is a thin DIRECT-mode client for the standalone / hosted
# sandbox-server REST API. It mirrors the Python SandboxServer
# (sdk/python/mitos/direct.py) and the TypeScript SandboxServer
# (sdk/typescript/src/server.ts). The Kubernetes / cluster mode (the controller,
# forkd, and the CRDs) is served by the Python and TypeScript SDKs only and is
# NOT part of this gem.
#
# Quickstart:
#
#   require "mitos"
#
#   server = Mitos.server                # base URL and API key from the env
#   server.create_template("python")
#   sandbox = server.fork("python")
#   puts sandbox.exec("echo hi").stdout
#   sandbox.terminate
module Mitos
  # Builds a SandboxServer from the resolved base URL and API key. The base URL
  # comes from +url+, else ENV['MITOS_BASE_URL'], else https://mitos.run; the
  # api_key from +api_key+, else ENV['MITOS_API_KEY']. This is the canonical
  # entry point: the standalone server has no template/fork bootstrap helper, so
  # callers create the template and fork explicitly.
  def self.server(url: nil, api_key: nil)
    SandboxServer.new(url: url, api_key: api_key)
  end
end
