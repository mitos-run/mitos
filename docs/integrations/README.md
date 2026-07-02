# Integrations

Drop a Mitos snapshot-fork microVM into the coding agent or agent framework you
already use. There are two shapes:

- **Coding agents that run on a sandbox** (Claude Code, opencode): point the agent
  at the Mitos MCP server, or host the agent harness inside a sandbox and reach it
  over HTTP.
- **Frameworks that use a sandbox as a tool** (OpenAI Agents SDK, LangChain /
  deepagents, Vercel AI SDK, Pydantic AI, AutoGen, LlamaIndex): bind the sandbox
  ops as tools, via a native adapter or the standard MCP server.

Every path runs on your own infrastructure (the standalone `sandbox-server`, no
Kubernetes, or a self-hosted cluster) or the hosted endpoint, and every path
reaches the same native ops: `exec`, `run_code`, `files`, and `fork`.

## Coding agents

| Agent | How | Doc |
|---|---|---|
| Claude Code | MCP server (`claude mcp add`) + agent skill | [claude-code.md](claude-code.md) |
| opencode | MCP server (`opencode.json`) or harness-in-sandbox | [opencode.md](opencode.md) |

## Frameworks

| Framework | How | Doc |
|---|---|---|
| OpenAI Agents SDK | native adapter `MitosSandboxTools` | [openai-agents.md](openai-agents.md) |
| LangChain / deepagents | native adapter `from mitos.integrations.langchain import MitosSandbox` | [../../sdk/python/README.md](../../sdk/python/README.md) |
| Vercel AI SDK, Pydantic AI, AutoGen, LlamaIndex | standard MCP server | [mcp-frameworks.md](mcp-frameworks.md) |
| Any MCP client | standard MCP server `mitos-mcp` | [../mcp.md](../mcp.md) |

## Migrating

| From | How | Doc |
|---|---|---|
| E2B | one-import shim `from mitos.e2b import Sandbox` | [../migrating-from-e2b.md](../migrating-from-e2b.md) |
| Daytona | one-import shim `from mitos.daytona import Daytona` | [../migrating-from-daytona.md](../migrating-from-daytona.md) |

## The honest Codex note

The Codex CLI is closed and runs on OpenAI's own containers; its sandbox cannot
be swapped for a Mitos one. The supported path into the OpenAI ecosystem is the
OpenAI Agents SDK ([openai-agents.md](openai-agents.md)), which takes a Mitos
sandbox as a tool.
