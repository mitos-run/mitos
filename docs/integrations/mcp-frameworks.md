# Run code in a Mitos sandbox from any MCP framework

Mitos ships a standard Model Context Protocol server, [`mitos-mcp`](../mcp.md),
that exposes the sandbox lifecycle as MCP tools (`sandbox_create`,
`sandbox_exec`, `sandbox_read_file`, `sandbox_write_file`, `sandbox_fork`,
`sandbox_terminate`). Any agent framework with an MCP client can drive a Mitos
sandbox with no Mitos-specific adapter code: point its MCP client at `mitos-mcp`
and the tools appear to the agent.

This page is the idiomatic MCP recipe for four common frameworks. Each snippet
spawns `mitos-mcp` over stdio; configure where it runs and which credential it
uses with two environment variables (see [docs/mcp.md](../mcp.md)):

- `MITOS_BASE_URL`: the sandbox-server base URL (defaults to `https://mitos.run`;
  set it to your self-hosted or local standalone server).
- `MITOS_API_KEY`: the bearer token (or run `mitos auth login` once and omit it).

Install the binary so it is on `PATH` (`go install mitos.run/mitos/cmd/mitos-mcp@latest`,
or use the released binary). The framework MCP APIs below move between versions,
so each recipe links the framework's own MCP docs as the source of truth.

## Vercel AI SDK (TypeScript)

Official docs: <https://ai-sdk.dev/docs/ai-sdk-core/mcp-tools>. The MCP imports
moved between AI SDK versions (`ai` / `ai/mcp-stdio` in v4, the dedicated
`@ai-sdk/mcp` package in newer releases); use the import your installed version
documents.

```ts
import { experimental_createMCPClient as createMCPClient } from "ai";
import { Experimental_StdioMCPTransport } from "ai/mcp-stdio";
import { generateText } from "ai";

const mcp = await createMCPClient({
  transport: new Experimental_StdioMCPTransport({
    command: "mitos-mcp",
    env: { MITOS_BASE_URL: process.env.MITOS_BASE_URL!, MITOS_API_KEY: process.env.MITOS_API_KEY! },
  }),
});

try {
  const tools = await mcp.tools(); // sandbox_create, sandbox_exec, ...
  const { text } = await generateText({
    model: yourModel,
    tools,
    prompt: "Create a python sandbox and run print(40+2).",
  });
  console.log(text);
} finally {
  await mcp.close();
}
```

## Pydantic AI (Python)

Official docs: <https://ai.pydantic.dev/mcp/client/>. Pydantic AI attaches an MCP
server as a toolset; the stdio transport spawns the subprocess and is managed by
`async with agent`.

```python
from fastmcp.client.transports import StdioTransport
from pydantic_ai import Agent
from pydantic_ai.mcp import MCPToolset

toolset = MCPToolset(
    StdioTransport(
        command="mitos-mcp",
        env={"MITOS_BASE_URL": "http://localhost:8080", "MITOS_API_KEY": "..."},
    )
)
agent = Agent("openai:gpt-4o", toolsets=[toolset])

async def main():
    async with agent:  # starts/stops the mitos-mcp subprocess
        result = await agent.run("Create a python sandbox and run print(40+2).")
        print(result.output)
```

## AutoGen (Python)

Official docs: <https://microsoft.github.io/autogen/stable/> (`autogen_ext.tools.mcp`).
`mcp_server_tools` adapts the MCP tools into AutoGen tools.

```python
import asyncio
from autogen_ext.tools.mcp import StdioServerParams, mcp_server_tools
from autogen_ext.models.openai import OpenAIChatCompletionClient
from autogen_agentchat.agents import AssistantAgent

async def main():
    params = StdioServerParams(
        command="mitos-mcp",
        env={"MITOS_BASE_URL": "http://localhost:8080", "MITOS_API_KEY": "..."},
        read_timeout_seconds=60,
    )
    tools = await mcp_server_tools(params)  # sandbox_create, sandbox_exec, ...
    agent = AssistantAgent(
        name="assistant",
        model_client=OpenAIChatCompletionClient(model="gpt-4o"),
        tools=tools,
    )
    await agent.run(task="Create a python sandbox and run print(40+2).")

asyncio.run(main())
```

## LlamaIndex (Python)

Official docs: <https://docs.llamaindex.ai/en/stable/api_reference/tools/mcp/>
(`pip install llama-index-tools-mcp`). `BasicMCPClient` spawns the stdio server;
`McpToolSpec` turns its tools into LlamaIndex tools.

```python
from llama_index.tools.mcp import BasicMCPClient, McpToolSpec
from llama_index.core.agent.workflow import FunctionAgent
from llama_index.llms.openai import OpenAI

client = BasicMCPClient(
    command_or_url="mitos-mcp",
    args=[],
    env={"MITOS_BASE_URL": "http://localhost:8080", "MITOS_API_KEY": "..."},
)
tools = McpToolSpec(client=client).to_tool_list()  # sandbox_create, sandbox_exec, ...

agent = FunctionAgent(tools=tools, llm=OpenAI(model="gpt-4o"))
# await agent.run("Create a python sandbox and run print(40+2).")
```

## Native SDK adapters (when you want code, not a tool server)

For frameworks Mitos ships a first-class adapter for, prefer that over MCP:
LangChain / deepagents (`mitos.integrations.langchain`), OpenAI Agents SDK
(`mitos.integrations.openai_agents`), Claude Agent SDK
(`mitos.integrations.claude_agent`), plus the E2B drop-in shim
(`mitos.e2b`). The MCP recipes above cover the long tail without per-framework
code.
