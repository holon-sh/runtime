import process from "node:process";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { AsyncLocalStorage } from "node:async_hooks";
import { fileURLToPath } from "node:url";
import { toNodeHandler } from "@modelcontextprotocol/node";

import type { AuthInfo, McpHttpHandler } from "@modelcontextprotocol/server";

type Application = {
  default: Pick<McpHttpHandler, "fetch" | "close">;
  frontend?: URL;
};

export async function start(loadApplication: () => Promise<Application>) {
  const host = process.env.HOLON_HOST_URL;
  if (!host) throw new Error("Application requires the Holon host");
  const nativeFetch = globalThis.fetch.bind(globalThis);
  const storage = new AsyncLocalStorage<string>();
  const broker = new RegExp(`^${host.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}/peer/[^/?#]+$`);

  globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
    const request = new Request(input as Request, init);
    const { headers } = request;
    if (!broker.test(request.url)) {
      for (const name of [...headers.keys()])
        if (name.startsWith("x-holon-")) headers.delete(name);
      return nativeFetch(request);
    }
    headers.append("x-holon-scope", storage.getStore() ?? "");
    const response = await nativeFetch(request);
    if (response.headers.has("x-holon-error")) {
      await response.body?.cancel();
      throw new Error("Holon peer call rejected or cancelled");
    }
    return response;
  }) as typeof globalThis.fetch;

  const { default: handler, frontend } = await loadApplication();
  if (typeof handler?.fetch !== "function" || typeof handler.close !== "function")
    throw new Error("Application must default-export an official MCP SDK handler");
  let directory: string | undefined;
  if (frontend !== undefined) {
    if (!(frontend instanceof URL) || frontend.search || frontend.hash)
      throw new Error("frontend must be a file URL for an application directory");
    directory = fileURLToPath(frontend);
  }

  const fetch = async (request: Request) => {
    const scope = request.headers.get("x-holon-scope");
    if (!scope) return new Response("Missing host context", { status: 403 });
    const context = await nativeFetch(`${host}/context/${encodeURIComponent(scope)}`, {
      signal: request.signal,
    });
    if (!context.ok) throw new Error("Holon request context unavailable");
    const identity = await context.json() as (Omit<AuthInfo, "resource"> & { resource: string }) | null;
    const authInfo = identity ? {
      ...identity,
      // An app's call made outside a request carries no context, so names no resource.
      resource: identity.resource ? new URL(identity.resource) : undefined,
    } : undefined;
    const headers = new Headers(request.headers);
    headers.delete("x-holon-scope");
    const response = await storage.run(scope, () =>
      handler.fetch(new Request(request, { headers }), { authInfo }),
    );
    if (!response.body) return response;
    const reader = response.body.getReader();
    const abort = () => void reader.cancel(request.signal.reason).catch(() => {});
    request.signal.addEventListener("abort", abort, { once: true });
    if (request.signal.aborted) abort();
    void reader.closed.finally(() => request.signal.removeEventListener("abort", abort)).catch(() => {});
    return new Response(
      new ReadableStream({
        async pull(controller) {
          const { done, value } = await storage.run(scope, () => reader.read());
          if (done) controller.close();
          else controller.enqueue(value);
        },
        cancel(reason) {
          return reader.cancel(reason);
        },
      }),
      response,
    );
  };
  let port: number | undefined, stop: () => void;
  if (globalThis.Bun) {
    const server = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch });
    port = server.port;
    stop = () => server.stop(true);
  } else {
    const dispatch = toNodeHandler({ fetch });
    const server = createServer((request, response) => {
      void dispatch(request, response).catch(() => response.destroy());
    });
    await new Promise<void>((resolve, reject) => {
      server.once("error", reject);
      server.listen(0, "127.0.0.1", resolve);
    });
    port = (server.address() as AddressInfo).port;
    stop = () => {
      server.close();
      server.closeAllConnections();
    };
  }
  let stopping = false;
  const shutdown = async () => {
    if (stopping) return;
    stopping = true;
    stop();
    await handler.close();
  };
  const signal = () => void shutdown().catch((error) => {
    console.error(error);
    process.exitCode = 1;
  });
  process.once("SIGTERM", signal);
  process.once("SIGINT", signal);
  try {
    const response = await nativeFetch(`${host}/ready`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ port, frontend: directory }),
    });
    if (!response.ok) throw new Error("Holon host rejected application startup");
  } catch (error) {
    await shutdown();
    throw error;
  }
}
