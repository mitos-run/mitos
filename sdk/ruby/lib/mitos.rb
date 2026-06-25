# frozen_string_literal: true

require "mitos/version"
require "mitos/errors"
require "mitos/sandbox"
require "mitos/sandbox_server"
require "mitos/k8s"
require "mitos/cluster_sandbox"
require "mitos/workspace"
require "mitos/agent_run"

# Mitos is the official Ruby SDK for mitos snapshot-fork sandboxes.
#
# It ships two modes, both dependency free (Ruby standard library only):
#
# - DIRECT mode (Mitos.server / Mitos::SandboxServer): a thin client for the
#   standalone / hosted sandbox-server REST API. Mirrors the Python
#   SandboxServer (sdk/python/mitos/direct.py) and the TypeScript SandboxServer.
# - CLUSTER mode (Mitos::AgentRun): drives the Kubernetes mitos.run CRDs
#   (SandboxPool, Sandbox, Workspace) over the Kubernetes REST API, with no
#   Kubernetes client gem. Mirrors the Python AgentRun (sdk/python/mitos/client.py)
#   and the TypeScript AgentRun.
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

  # Builds an AgentRun, the Kubernetes cluster-mode client that drives the
  # mitos.run CRDs. With in_cluster: true the config comes from the pod
  # service-account mount; otherwise from a kubeconfig (kubeconfig: path, else
  # KUBECONFIG, else ~/.kube/config). namespace defaults to the kubeconfig /
  # in-cluster namespace, else "default".
  #
  #   run = Mitos.cluster(namespace: "agents")
  #   sb  = run.sandbox(image: "python:3.12", ready: true)
  def self.cluster(namespace: nil, kubeconfig: nil, in_cluster: false, allow_default_pool: true)
    AgentRun.new(
      namespace: namespace, kubeconfig: kubeconfig,
      in_cluster: in_cluster, allow_default_pool: allow_default_pool
    )
  end
end
