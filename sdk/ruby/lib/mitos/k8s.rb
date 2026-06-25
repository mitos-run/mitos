# frozen_string_literal: true

require "base64"
require "json"
require "net/http"
require "openssl"
require "uri"
require "yaml"

require "mitos/errors"

module Mitos
  # A minimal Kubernetes REST client built on the Ruby standard library only
  # (net/http for transport, openssl for TLS and the CA bundle, json for the
  # bodies, yaml for kubeconfig parsing, base64 for the in-cluster and secret
  # token decoding). It exists so the Ruby SDK's cluster mode stays dependency
  # free: there is no equivalent of the Python "kubernetes" client or the
  # TypeScript "@kubernetes/client-node" here, just direct REST against the API
  # server.
  #
  # The client speaks only the namespaced custom-resource and Secret endpoints
  # the AgentRun surface needs:
  #
  #   GET    /apis/{group}/{version}/namespaces/{ns}/{plural}
  #   GET    /apis/{group}/{version}/namespaces/{ns}/{plural}/{name}
  #   POST   /apis/{group}/{version}/namespaces/{ns}/{plural}
  #   DELETE /apis/{group}/{version}/namespaces/{ns}/{plural}/{name}
  #   GET    /api/v1/namespaces/{ns}/secrets/{name}
  #
  # Auth is a bearer token (in-cluster service-account token or a kubeconfig
  # token) or client-certificate mTLS (a kubeconfig client cert and key). The
  # token VALUE is never logged and never placed in an error message.
  class K8sClient
    # The in-cluster service-account mount, mounted into every pod by the
    # kubelet. The token rotates; it is re-read on each request so a rotated
    # token is picked up without recreating the client.
    SA_DIR = "/var/run/secrets/kubernetes.io/serviceaccount"
    SA_TOKEN = "#{SA_DIR}/token"
    SA_CA = "#{SA_DIR}/ca.crt"
    SA_NAMESPACE = "#{SA_DIR}/namespace"

    attr_reader :server, :default_namespace

    # Build the client from a resolved config Hash (see .in_cluster and
    # .from_kubeconfig). The config carries: :server (the API base URL),
    # :ca_data (PEM string or nil for the system store), :token (bearer, or
    # nil), :token_file (a path re-read per request, or nil), :client_cert_data
    # and :client_key_data (PEM strings for mTLS, or nil), :insecure (skip TLS
    # verification, only honored when a kubeconfig explicitly sets it), and
    # :namespace (the default namespace, or nil).
    def initialize(config)
      @server = config[:server].to_s.sub(%r{/+\z}, "")
      @ca_data = config[:ca_data]
      @token = config[:token]
      @token_file = config[:token_file]
      @client_cert_data = config[:client_cert_data]
      @client_key_data = config[:client_key_data]
      @insecure = config[:insecure] ? true : false
      @default_namespace = config[:namespace]
      raise config_error("the resolved Kubernetes server URL is empty") if @server.empty?
    end

    # Resolve the in-cluster configuration from the service-account mount and
    # the KUBERNETES_SERVICE_HOST / KUBERNETES_SERVICE_PORT environment the
    # kubelet injects. Raises a typed MitosError naming the missing piece when
    # not running inside a pod.
    def self.in_cluster
      host = ENV["KUBERNETES_SERVICE_HOST"]
      port = ENV["KUBERNETES_SERVICE_PORT"]
      if host.nil? || host.empty? || port.nil? || port.empty?
        raise MitosError.new(
          "not running in a Kubernetes cluster",
          code: "k8s_config_error",
          cause: "KUBERNETES_SERVICE_HOST / KUBERNETES_SERVICE_PORT are not set",
          remediation: "Run inside a pod with a service account, or pass kubeconfig: to AgentRun."
        )
      end
      unless File.file?(SA_TOKEN)
        raise MitosError.new(
          "in-cluster service-account token is not mounted",
          code: "k8s_config_error",
          cause: "#{SA_TOKEN} is absent",
          remediation: "Run inside a pod whose service account token is mounted, or pass kubeconfig:."
        )
      end
      ca = File.file?(SA_CA) ? File.read(SA_CA) : nil
      ns = File.file?(SA_NAMESPACE) ? File.read(SA_NAMESPACE).strip : nil
      new(
        server: "https://#{host}:#{port}",
        ca_data: ca,
        token_file: SA_TOKEN,
        namespace: (ns unless ns.nil? || ns.empty?)
      )
    end

    # Resolve configuration from a kubeconfig YAML file. +path+ defaults to
    # ENV['KUBECONFIG'] (first entry if it is a list) then ~/.kube/config. The
    # current-context cluster (server, CA) and user (token or client cert/key)
    # are read; relative file references inside the kubeconfig are resolved and
    # inlined. Raises a typed MitosError when the file or the named context is
    # missing.
    def self.from_kubeconfig(path = nil)
      resolved = resolve_kubeconfig_path(path)
      unless resolved && File.file?(resolved)
        raise MitosError.new(
          "kubeconfig not found",
          code: "k8s_config_error",
          cause: "no kubeconfig at #{resolved || '(unset)'}",
          remediation: "Set KUBECONFIG, pass kubeconfig: to AgentRun, or run AgentRun(in_cluster: true) in a pod."
        )
      end
      doc = YAML.safe_load(File.read(resolved), permitted_classes: [], aliases: false) || {}
      new(parse_kubeconfig(doc, File.dirname(File.expand_path(resolved))))
    end

    def self.resolve_kubeconfig_path(path)
      return path unless path.nil? || (path.respond_to?(:empty?) && path.empty?)

      env = ENV["KUBECONFIG"]
      unless env.nil? || env.empty?
        # KUBECONFIG may be a path list; the first existing entry wins, matching
        # the client-go merge order closely enough for the single-file case.
        first = env.split(File::PATH_SEPARATOR).find { |p| !p.empty? }
        return first if first
      end
      home = Dir.home
      return nil if home.nil? || home.empty?

      File.join(home, ".kube", "config")
    rescue StandardError
      nil
    end

    # Turn a parsed kubeconfig document into the config Hash initialize expects.
    # base_dir resolves relative certificate-authority / client-certificate /
    # token file references.
    def self.parse_kubeconfig(doc, base_dir)
      current = doc["current-context"]
      raise config_error_class("kubeconfig has no current-context") if current.nil? || current.to_s.empty?

      context = find_named(doc["contexts"], current)
      raise config_error_class("kubeconfig context #{current} not found") unless context

      ctx = context["context"] || {}
      cluster = find_named(doc["clusters"], ctx["cluster"]) || {}
      user = find_named(doc["users"], ctx["user"]) || {}
      c = cluster["cluster"] || {}
      u = user["user"] || {}

      {
        server: c["server"],
        ca_data: read_inline_or_file(c["certificate-authority-data"], c["certificate-authority"], base_dir),
        token: kubeconfig_token(u),
        client_cert_data: read_inline_or_file(u["client-certificate-data"], u["client-certificate"], base_dir),
        client_key_data: read_inline_or_file(u["client-key-data"], u["client-key"], base_dir),
        insecure: c["insecure-skip-tls-verify"] ? true : false,
        namespace: (ctx["namespace"] unless ctx["namespace"].nil? || ctx["namespace"].to_s.empty?)
      }
    end

    # Read a base64 "*-data" field if present, else read the referenced file
    # (relative paths resolved against the kubeconfig directory). Returns a PEM
    # / raw string or nil.
    def self.read_inline_or_file(data_b64, file_ref, base_dir)
      unless data_b64.nil? || data_b64.to_s.empty?
        return Base64.decode64(data_b64)
      end
      return nil if file_ref.nil? || file_ref.to_s.empty?

      path = File.expand_path(file_ref, base_dir)
      File.file?(path) ? File.read(path) : nil
    end

    # The kubeconfig user token: an inline "token", else a "tokenFile" read from
    # disk. Exec-plugin and auth-provider credential helpers are out of scope
    # for the dependency-free client; their absence surfaces as a 401 from the
    # API server with an actionable message rather than a silent tokenless call.
    def self.kubeconfig_token(user)
      tok = user["token"]
      return tok unless tok.nil? || tok.to_s.empty?

      tf = user["tokenFile"]
      return File.read(tf).strip if !tf.nil? && !tf.to_s.empty? && File.file?(tf)

      nil
    end

    def self.find_named(list, name)
      return nil unless list.is_a?(Array)

      list.find { |e| e.is_a?(Hash) && e["name"] == name }
    end

    def self.config_error_class(detail)
      MitosError.new(
        "invalid kubeconfig",
        code: "k8s_config_error",
        cause: detail,
        remediation: "Point KUBECONFIG / kubeconfig: at a valid config, or run AgentRun(in_cluster: true) in a pod."
      )
    end

    # GET a namespaced custom-object collection. Returns the decoded list body
    # (a Hash with an "items" array).
    def list_namespaced(group, version, namespace, plural)
      request(Net::HTTP::Get, custom_path(group, version, namespace, plural), nil)
    end

    # GET a single namespaced custom object by name. Returns the decoded body.
    def get_namespaced(group, version, namespace, plural, name)
      request(Net::HTTP::Get, custom_path(group, version, namespace, plural, name), nil)
    end

    # POST a namespaced custom object. +body+ is the object Hash. Returns the
    # decoded created body.
    def create_namespaced(group, version, namespace, plural, body)
      request(Net::HTTP::Post, custom_path(group, version, namespace, plural), body)
    end

    # DELETE a namespaced custom object by name. Returns the decoded body.
    def delete_namespaced(group, version, namespace, plural, name)
      request(Net::HTTP::Delete, custom_path(group, version, namespace, plural, name), nil)
    end

    # GET a core/v1 Secret by name. Returns the decoded Secret body (with the
    # base64 "data" map). The caller decodes only the key it needs and never
    # logs the value.
    def get_secret(namespace, name)
      path = "/api/v1/namespaces/#{esc(namespace)}/secrets/#{esc(name)}"
      request(Net::HTTP::Get, path, nil)
    end

    # The HTTP status of the last raised ApiError, exposed so callers can branch
    # on 404 / 409 without parsing a message. Set on the exception, not here.

    private

    def custom_path(group, version, namespace, plural, name = nil)
      base = "/apis/#{esc(group)}/#{esc(version)}/namespaces/#{esc(namespace)}/#{esc(plural)}"
      name.nil? || name.empty? ? base : "#{base}/#{esc(name)}"
    end

    def esc(segment)
      URI.encode_www_form_component(segment.to_s)
    end

    # Perform the request: build the URI, attach auth (bearer or mTLS), set the
    # TLS verification mode and CA, send the JSON body, and decode the JSON
    # response. A non-2xx status raises a typed ApiError carrying the parsed
    # Kubernetes Status (reason + code) so callers branch on .status, never the
    # message. Auth material is never logged.
    def request(method_class, path, body)
      uri = URI.parse(@server + path)
      req = method_class.new(uri.request_uri)
      req["Accept"] = "application/json"
      if body
        req["Content-Type"] = "application/json"
        req.body = JSON.generate(body)
      end
      apply_auth(req)

      http = build_http(uri)
      resp = http.request(req)
      status = resp.code.to_i
      raise api_error(status, resp.body) unless status >= 200 && status < 300

      text = resp.body
      return {} if text.nil? || text.empty?

      JSON.parse(text)
    end

    # Attach the bearer token. The token file is re-read per request so a
    # rotated in-cluster token is honored. mTLS users carry no bearer; the cert
    # is attached on the transport in build_http.
    def apply_auth(req)
      token = current_token
      req["Authorization"] = "Bearer #{token}" if token && !token.empty?
    end

    def current_token
      return @token if @token && !@token.empty?
      return nil if @token_file.nil? || @token_file.empty?

      File.read(@token_file).strip
    rescue StandardError
      nil
    end

    def build_http(uri)
      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = (uri.scheme == "https")
      return http unless uri.scheme == "https"

      if @insecure
        http.verify_mode = OpenSSL::SSL::VERIFY_NONE
      else
        http.verify_mode = OpenSSL::SSL::VERIFY_PEER
        if @ca_data && !@ca_data.empty?
          store = OpenSSL::X509::Store.new
          store.add_cert(OpenSSL::X509::Certificate.new(@ca_data))
          http.cert_store = store
        end
      end
      if @client_cert_data && !@client_cert_data.empty? && @client_key_data && !@client_key_data.empty?
        http.cert = OpenSSL::X509::Certificate.new(@client_cert_data)
        http.key = OpenSSL::PKey.read(@client_key_data)
      end
      http
    end

    # Build a typed ApiError from a non-2xx response, preferring the Kubernetes
    # Status object {kind:"Status", reason, code, message}. The token VALUE is
    # never echoed: only the API server's own Status text is surfaced.
    def api_error(status, raw_body)
      reason = nil
      message = "Kubernetes API request failed: HTTP #{status}"
      parsed = begin
        JSON.parse(raw_body.to_s)
      rescue StandardError
        nil
      end
      if parsed.is_a?(Hash) && parsed["kind"] == "Status"
        reason = parsed["reason"]
        message = parsed["message"] unless parsed["message"].nil? || parsed["message"].to_s.empty?
      end
      ApiError.new(message, status: status, reason: reason)
    end

    def config_error(detail)
      MitosError.new(
        "invalid Kubernetes client configuration",
        code: "k8s_config_error",
        cause: detail,
        remediation: "Pass a valid kubeconfig: or run AgentRun(in_cluster: true) inside a pod."
      )
    end
  end

  # ApiError is raised for a non-2xx Kubernetes API response. +status+ is the
  # HTTP status (callers branch on 404 / 409, never the message) and +reason+ is
  # the Kubernetes Status reason (for example "NotFound", "AlreadyExists"). It is
  # a MitosError so a caller that only rescues Mitos::MitosError still catches it.
  class ApiError < MitosError
    attr_reader :reason

    def initialize(message, status:, reason: nil)
      super(
        message,
        code: "k8s_api_error",
        cause: reason.to_s,
        remediation: "Inspect the Kubernetes API server response and RBAC for this resource.",
        status: status
      )
      @reason = reason
    end
  end
end
