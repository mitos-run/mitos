# frozen_string_literal: true

module Mitos
  # MitosError is the LLM-legible error raised by the SDK. It mirrors the server
  # envelope {error:{code, message, cause, remediation}} and the Python
  # AgentRunError / TypeScript AgentRunError. +code+ is a stable machine
  # identifier; +cause+ is the underlying detail; +remediation+ is a short
  # actionable hint; +status+ is the HTTP status when the error came from a
  # response. No bearer token or secret value ever appears in any field: the
  # SDK redacts the configured api_key from the body before it becomes a cause.
  #
  # Callers branch on +code+, never on the message text.
  class MitosError < StandardError
    attr_reader :code, :cause_detail, :remediation, :status

    def initialize(message, code:, cause: "", remediation: "", status: nil)
      super(message)
      @code = code
      @cause_detail = cause
      @remediation = remediation
      @status = status
    end

    def to_s
      parts = ["[#{@code}] #{super}"]
      parts << "cause: #{@cause_detail}" unless @cause_detail.nil? || @cause_detail.empty?
      unless @remediation.nil? || @remediation.empty?
        parts << "remediation: #{@remediation}"
      end
      parts.join(" | ")
    end
  end
end
