# frozen_string_literal: true

require "base64"

module Mitos
  # The result of a Sandbox#exec call. Mirrors the Python ExecResult: it carries
  # {exit_code, stdout, stderr, exec_time_ms} drained from the Connect ExecStream
  # response (stdout / stderr / exit frames).
  ExecResult = Struct.new(:exit_code, :stdout, :stderr, :exec_time_ms, keyword_init: true) do
    def success?
      exit_code.zero?
    end
  end

  # A sandbox handle returned by SandboxServer#fork. exec round-trips through the
  # Connect sandbox.v1.Sandbox runtime protocol (the ExecStream RPC) and
  # terminate issues DELETE /v1/sandboxes/{id}. The handle holds the
  # SandboxServer it was forked from so requests carry the same base URL and
  # optional bearer header.
  class Sandbox
    attr_reader :id, :endpoint

    def initialize(id:, endpoint:, server:)
      @id = id
      @endpoint = endpoint
      @server = server
    end

    # Runs +command+ in the sandbox over the Connect ExecStream RPC and returns
    # an ExecResult. The server streams stdout and stderr frames followed by an
    # exit frame; this drains them into the result. Requires a Ready sandbox: the
    # sandbox-server routes exec through the guest agent over vsock, so a sandbox
    # that is not yet up returns a typed error.
    def exec(command, timeout: 30)
      message = { "command" => command }
      message["timeoutSeconds"] = timeout if timeout && timeout > 0

      stdout = +"".b
      stderr = +"".b
      exit_code = 0
      exec_time_ms = 0

      client = @server.connect_client(@id)
      client.server_stream("ExecStream", message) do |response|
        if response.key?("stdout")
          stdout << Base64.decode64(response["stdout"].to_s)
        elsif response.key?("stderr")
          stderr << Base64.decode64(response["stderr"].to_s)
        elsif response.key?("exit")
          exit_info = response["exit"] || {}
          exit_code = exit_info["exitCode"] || 0
          exec_time_ms = exit_info["execTimeMs"] || 0
        end
      end

      ExecResult.new(
        exit_code: exit_code,
        stdout: stdout.force_encoding(Encoding::UTF_8),
        stderr: stderr.force_encoding(Encoding::UTF_8),
        exec_time_ms: exec_time_ms
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
