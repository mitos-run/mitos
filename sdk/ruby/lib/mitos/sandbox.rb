# frozen_string_literal: true

require "net/http"

module Mitos
  # The result of a Sandbox#exec call. Mirrors the Python ExecResult and the
  # sandbox-server /v1/exec response {exit_code, stdout, stderr, exec_time_ms}.
  ExecResult = Struct.new(:exit_code, :stdout, :stderr, :exec_time_ms, keyword_init: true) do
    def success?
      exit_code.zero?
    end
  end

  # A sandbox handle returned by SandboxServer#fork. exec round-trips through the
  # server URL (POST /v1/exec) and terminate issues DELETE /v1/sandboxes/{id}.
  # The handle holds the SandboxServer it was forked from so requests carry the
  # same base URL and optional bearer header.
  class Sandbox
    attr_reader :id, :endpoint

    def initialize(id:, endpoint:, server:)
      @id = id
      @endpoint = endpoint
      @server = server
    end

    # Runs +command+ in the sandbox and returns an ExecResult. Requires a Ready
    # sandbox: the sandbox-server routes exec through the guest agent over vsock,
    # so a sandbox that is not yet up returns a typed error.
    def exec(command, timeout: 30)
      body = { "sandbox" => @id, "command" => command, "timeout" => timeout }
      data = @server.request_json(Net::HTTP::Post, "/v1/exec", body, {})
      ExecResult.new(
        exit_code: data["exit_code"],
        stdout: data["stdout"] || "",
        stderr: data["stderr"] || "",
        exec_time_ms: data["exec_time_ms"] || 0
      )
    end

    # Terminates the sandbox via DELETE /v1/sandboxes/{id}.
    def terminate
      @server.terminate(@id)
    end

    def to_s
      "#<Mitos::Sandbox id=#{@id.inspect} endpoint=#{@endpoint.inspect}>"
    end
    alias inspect to_s
  end
end
