import process from "node:process";
import { once } from "node:events";
import { createInterface } from "node:readline";
import {
  Client,
  StreamableHTTPClientTransport,
} from "@modelcontextprotocol/client";
import {
  createMcpHandler,
  McpServer,
  fromJsonSchema,
} from "@modelcontextprotocol/server";

const gates = {
  inspect: Promise.withResolvers(),
  stream: Promise.withResolvers(),
};

const commands = createInterface({ input: process.stdin });
commands.on("line", (line) => {
  const { release } = JSON.parse(line);
  gates[release].resolve();
});
const emit = (event, extra = {}) => console.log(JSON.stringify({ event, ...extra }));
const inputSchema = fromJsonSchema({ type: "object", additionalProperties: false });

async function callPeer(name) {
  const client = new Client(
    { name: "ordinary-peer", version: "1" },
    { versionNegotiation: { mode: { pin: "2026-07-28" } } },
  );
  try {
    await client.connect(
      new StreamableHTTPClientTransport(new URL(process.env.PEER_MCP_URL)),
    );
    return await client.callTool({ name, arguments: {} });
  } finally {
    await client.close();
  }
}

const handler = createMcpHandler(
  ({ requestInfo }) => {
    const server = new McpServer({ name: "ordinary-app", version: "1" });
    server.registerTool("external", { inputSchema }, async () => {
      const response = await fetch(process.env.EXTERNAL_URL, {
        headers: {
          "x-holon-context": "must-not-leak",
          "x-holon-other": "must-not-leak",
        },
      });
      return { content: [{ type: "text", text: await response.text() }] };
    });
    server.registerTool("credentials", { inputSchema }, async () => {
      const response = await fetch(process.env.PEER_MCP_URL, {
        headers: { authorization: "Bearer app-token" },
      });
      await response.body?.cancel();
      return { content: [] };
    });
    server.registerTool("redirect", { inputSchema }, async () => {
      const response = await fetch(process.env.PEER_MCP_URL);
      await response.body?.cancel();
      return { content: [] };
    });
    server.registerTool("wait", { inputSchema }, async (_, ctx) => {
      if (process.env.PEER_MCP_URL) return callPeer("wait");
      emit("waiting");
      const signal = ctx.mcpReq.signal;
      try {
        signal.throwIfAborted();
        await once(signal, "abort");
        throw signal.reason;
      } finally {
        emit("cancelled");
      }
    });
    server.registerTool("inspect", { inputSchema }, async (_, ctx) => {
      if (process.env.PEER_MCP_URL) return callPeer("inspect");
      if (process.env.TEST_INSPECT_BARRIER) {
        emit("inspecting");
        await gates.inspect.promise;
      }
      return {
        content: [{
          type: "text",
          text: JSON.stringify({
            identity: ctx.http.authInfo ?? null,
            headers: [...requestInfo.headers.keys()],
          }),
        }],
      };
    });
    return server;
  },
  { legacy: "reject" },
);

export default {
  async fetch(request, options) {
    if (request.headers.get("x-test-stream") === "peer") {
      return new Response(
        new ReadableStream({
          start(controller) {
            controller.enqueue(new TextEncoder().encode("peer stream"));
          },
          cancel() {
            emit("peer-stream-closed");
          },
        }),
      );
    }
    if (request.headers.get("x-test-stream") === "source") {
      const peer = await fetch(process.env.PEER_MCP_URL, {
        method: "POST",
        headers: { "x-test-stream": "peer" },
      });
      emit("stream-peer-ready");
      void (async () => {
        await peer.text().catch(() => {});
        const result = await callPeer("inspect");
        emit("stream-late-forwarded", { identity: JSON.parse(result.content[0].text).identity });
      })();
      return new Response(
        new ReadableStream({
          start(controller) {
            controller.enqueue(new TextEncoder().encode("first chunk"));
            void gates.stream.promise.then(() => {
              controller.enqueue(new TextEncoder().encode("last chunk"));
              controller.close();
            });
          },
        }),
      );
    }
    return handler.fetch(request, options);
  },
  async close() {
    commands.close();
    process.stdin.destroy();
    await handler.close();
  },
};

export const frontend = process.env.TEST_FRONTEND ? new URL(process.env.TEST_FRONTEND) : undefined;
