import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer, request } from "node:http";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { createInterface } from "node:readline";
import { fileURLToPath, pathToFileURL } from "node:url";
import { cp, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { basename, join } from "node:path";
import {
  Client,
  StreamableHTTPClientTransport,
} from "@modelcontextprotocol/client";

const runtime = process.env.HOLON_TEST_RUNTIME ?? process.execPath;
const python = basename(runtime).startsWith("python");

async function listen(server) {
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  return server.address().port;
}
function host(env, options) {
  return spawn(
    fileURLToPath(new URL("./.host-test", import.meta.url)),
    [
      "--transport=hosted-http",
      "--",
      runtime,
      ...(runtime.endsWith("deno")
        ? ["run", "--allow-all", "--node-modules-dir=manual"]
        : []),
      fileURLToPath(new URL(python ? "../python/testdata/entry.py" : "./testdata/entry.mjs", import.meta.url)),
    ],
    {
      stdio: ["pipe", "pipe", "pipe"],
      env: { PATH: process.env.PATH, ...env },
      ...options,
    },
  );
}
async function app(t, peers = {}, extra = {}) {
  const reserve = createServer();
  const port = await listen(reserve);
  await new Promise((resolve) => reserve.close(resolve));
  const child = host({
    PORT: String(port),
    HOLON_MCP_RUNTIME_CONFIG: JSON.stringify({ version: 2, peers }),
    ...extra,
  });
  const messages = [];
  let stderr = "";
  child.stderr.on("data", (data) => (stderr += data));
  createInterface({ input: child.stdout }).on("line", (line) => {
    const message = JSON.parse(line);
    messages.push(message);
    child.emit("message", message);
  });
  t.after(async () => {
    if (stderr) t.diagnostic(stderr);
    if (child.exitCode === null && child.signalCode === null) {
      child.kill("SIGTERM");
      const force = setTimeout(() => child.kill("SIGKILL"), 2000);
      await once(child, "exit");
      clearTimeout(force);
    }
    assert.equal(
      child.exitCode,
      0,
      `shutdown failed (${child.signalCode}): ${stderr}`,
    );
  });
  await Promise.race([
    once(child, "message", { signal: t.signal }),
    once(child, "exit").then(() => {
      throw new Error(stderr);
    }),
  ]);
  return { child, messages, signal: t.signal, url: `http://127.0.0.1:${port}/mcp` };
}
function context(subject, principal = {}, hook) {
  const claims = {
    aud: ["https://holon.example"],
    exp: 1900000060.25,
    hook,
    ...(subject
      ? {
          principal: {
            iss: "https://issuer.example",
            sub: subject,
            client_id: "client",
            scope: " read  write\tcombined ",
            act: { iss: "https://issuer.example", sub: "actor" },
            ...principal,
          },
        }
      : {}),
  };
  return {
    "x-holon-context": `header.${Buffer.from(JSON.stringify(claims)).toString("base64url")}.signature`,
    "x-holon-caller": "spiffe://edge",
  };
}
async function call(url, name, headers, signal) {
  const client = new Client(
    { name: "test", version: "1" },
    { versionNegotiation: { mode: { pin: "2026-07-28" } } },
  );
  try {
    await client.connect(
      new StreamableHTTPClientTransport(new URL(url), {
        requestInit: { headers },
      }),
    );
    return await client.callTool({ name, arguments: {} }, { signal });
  } finally {
    await client.close();
  }
}
async function event(app, name, count = 1) {
  while (app.messages.filter((message) => message.event === name).length < count) {
    await once(app.child, "message", { signal: app.signal });
  }
  return app.messages.findLast((message) => message.event === name);
}
function release(app, name) {
  app.child.stdin.write(`${JSON.stringify({ release: name })}\n`);
}

test(
  "SDK applications preserve identity, containment and request lifetime",
  { timeout: 30000 },
  async (t) => {
    const downstream = await app(t, {}, { TEST_INSPECT_BARRIER: "true" });
    let redirect = false;
    const externalRequests = [];
    const external = createServer((req, res) => {
      externalRequests.push(req.headers);
      res.setHeader("x-holon-error", "external-service-error");
      res.end(JSON.stringify(req.headers));
    });
    const externalPort = await listen(external);
    t.after(() => external.close());
    const mesh = createServer((req, res) => {
      if (redirect) {
        res
          .writeHead(302, { location: `http://127.0.0.1:${externalPort}` })
          .end();
        return;
      }
      const forwarded = request(
        downstream.url,
        {
          method: req.method,
          headers: { ...req.headers, "x-holon-caller": "spiffe://app-a" },
        },
        (reply) => {
          res.writeHead(reply.statusCode, reply.headers);
          reply.pipe(res);
        },
      );
      forwarded.on("error", () => res.destroy());
      res.on("close", () => forwarded.destroy());
      req.pipe(forwarded);
    });
    const peer = `http://127.0.0.1:${await listen(mesh)}/mcp`;
    t.after(() => mesh.close());
    const upstream = await app(
      t,
      { peer },
      { PEER_MCP_URL: "http://stale.invalid/mcp", EXTERNAL_URL: `http://127.0.0.1:${externalPort}` },
    );
    const identities = [
      {
        subject: "alice",
        principal: {},
        actor: { act: { iss: "https://issuer.example", sub: "actor" } },
        scopes: ["read", "write\tcombined"],
      },
      {
        subject: "large-subject-" + "<".repeat(9000),
        principal: { act: null, scope: "" },
        actor: {},
        scopes: [],
      },
      {
        subject: "without-optional-fields",
        principal: { act: undefined, scope: undefined },
        actor: {},
        scopes: [],
      },
    ];
    const concurrent = Promise.all(
      identities.map(async ({ subject, principal, actor, scopes }) => {
        const headers = context(subject, principal);
        const result = await call(upstream.url, "inspect", headers);
        const observed = JSON.parse(result.content[0].text);
        assert(!observed.headers.some((name) => name.startsWith("x-holon-")));
        assert.deepEqual(observed.identity, {
          token: headers["x-holon-context"],
          extra: {
            sub: subject,
            caller: "spiffe://app-a",
            iss: "https://issuer.example",
            ...actor,
          },
          scopes,
          clientId: "client",
          expiresAt: python ? 1900000060 : 1900000060.25,
          resource: python ? "https://holon.example" : "https://holon.example/",
        });
      }),
    );
    await event(downstream, "inspecting", identities.length);
    release(downstream, "inspect");
    await concurrent;
    if (python) {
      const headers = context("alice");
      const transport = new StreamableHTTPClientTransport(new URL(downstream.url), {
        requestInit: { headers },
      });
      const client = new Client(
        { name: "legacy", version: "1" },
        { versionNegotiation: { mode: "legacy" } },
      );
      try {
        await client.connect(transport);
        Object.assign(headers, context("alice", { scope: "fresh", act: null }));
        const result = await client.callTool({ name: "inspect", arguments: {} });
        const { identity } = JSON.parse(result.content[0].text);
        assert.equal(identity.token, headers["x-holon-context"]);
        assert.deepEqual(identity.scopes, ["fresh"]);
        assert.equal(identity.extra.act, undefined);
      } finally {
        await client.close();
      }
      // The Python SDK's server span parents on the caller's trace header.
      const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";
      const traced = await call(downstream.url, "inspect", { ...context("alice"), traceparent });
      assert.equal(JSON.parse(traced.content[0].text).traceparent, traceparent);
    }
    const hook = {
      app: "accounts", job: "hook-job", jobUID: "job-uid",
      operation: "install", step: 1, attempt: 1, deadline: "2030-03-17T17:47:40Z",
    };
    const invoked = await call(upstream.url, "inspect", context(undefined, {}, hook));
    assert.deepEqual(JSON.parse(invoked.content[0].text).identity.extra, {
      caller: "spiffe://app-a",
      hook,
    });
    const response = await call(upstream.url, "external", context("user"));
    assert(
      !Object.keys(JSON.parse(response.content[0].text)).some((name) =>
        name.startsWith("x-holon-"),
      ),
    );
    const abort = new AbortController();
    const pending = call(upstream.url, "wait", context("user"), abort.signal);
    const rejected = assert.rejects(pending, /cancelled by host contract test/);
    await event(downstream, "waiting");
    abort.abort(new Error("cancelled by host contract test"));
    await rejected;
    await event(downstream, "cancelled");
    const streaming = await fetch(upstream.url, {
      method: "POST",
      headers: { ...context("stream-user"), "x-test-stream": "source" },
    });
    await event(upstream, "stream-peer-ready");
    release(upstream, "stream");
    await streaming.text();
    assert.equal((await event(upstream, "stream-late-forwarded")).identity, null);
    await event(downstream, "peer-stream-closed");
    redirect = true;
    externalRequests.length = 0;
    assert.equal(
      (await call(upstream.url, "redirect", context("user"))).isError,
      true,
    );
    assert.deepEqual(
      externalRequests,
      [],
      "peer redirect reached an external server",
    );
    redirect = false;
    const abandoned = call(
      upstream.url,
      "wait",
      context("shutdown-user"),
    ).catch(() => null);
    await event(downstream, "waiting", 2);
    const stopped = once(upstream.child, "exit");
    upstream.child.kill("SIGTERM");
    await stopped;
    await abandoned;
    await event(downstream, "cancelled", 2);
  },
);

test(
  "standalone SDK applications ignore untrusted identity and serve static files",
  { timeout: 10000 },
  async (t) => {
    const directory = await mkdtemp(join(process.cwd(), ".frontend-test-"));
    t.after(() => rm(directory, { recursive: true, force: true }));
    await cp(new URL("./testdata/web/", import.meta.url), join(directory, "web"), { recursive: true });
    await writeFile(join(directory, "secret"), "Outside frontend");
    await writeFile(join(directory, "web/.private"), "Hidden file");
    await symlink(join(directory, "secret"), join(directory, "web/escape"));

    const standalone = await app(
      t,
      {},
      {
        HOLON_MCP_RUNTIME_CONFIG: undefined,
        TEST_FRONTEND: pathToFileURL(join(directory, "web/")).href,
      },
    );
    const result = await call(standalone.url, "inspect", context("forged"));
    assert.equal(JSON.parse(result.content[0].text).identity, null);
    const home = new URL("/", standalone.url);
    const html = await (await fetch(home)).text();
    assert.match(html, /Application frontend/);
    for (const path of [
      "/%2eprivate",
      "/%2findex.html",
      "/missing",
      "/escape",
    ]) {
      assert.equal((await fetch(new URL(path, home))).status, 404);
    }
  },
);

test(
  "a Python application that fails to import shows only its own frames",
  { skip: !python, timeout: 10000 },
  async (t) => {
    const directory = await mkdtemp(join(process.cwd(), ".import-test-"));
    t.after(() => rm(directory, { recursive: true, force: true }));
    await writeFile(join(directory, "server.py"), "import helper\n");
    await writeFile(join(directory, "helper.py"), "def broken(:\n    pass\n");
    const child = host({}, { cwd: directory });
    let stderr = "";
    child.stderr.on("data", (data) => (stderr += data));
    const [code] = await once(child, "exit");
    assert.notEqual(code, 0);
    const frames = [...stderr.matchAll(/File "([^"]+)"/g)].map(([, file]) => basename(file));
    assert.deepEqual(frames, ["server.py", "helper.py"]);
    assert.match(stderr, /SyntaxError/);
  },
);
