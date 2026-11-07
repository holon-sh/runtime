import asyncio
import json
import os
import sys
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager, suppress
from pathlib import Path
from typing import Any
from urllib.parse import unquote, urlsplit

import httpx2
from holon.binding import resolve_default_binding
from mcp import Client
from mcp.client.streamable_http import streamable_http_client
from mcp.server.auth.middleware.auth_context import get_access_token
from mcp.server.mcpserver import Context, MCPServer
from mcp.types import CallToolResult, TextContent
from starlette.datastructures import Headers
from starlette.responses import StreamingResponse
from starlette.types import ASGIApp, Message, Receive, Scope, Send

from holon_host import http_client

gates = {name: asyncio.Event() for name in ("inspect", "stream")}
peer_binding = resolve_default_binding("peer")
http: httpx2.AsyncClient


def emit(event: str, **extra: object) -> None:
    print(json.dumps({"event": event, **extra}), flush=True)


@asynccontextmanager
async def lifespan(server: MCPServer[Any]) -> AsyncIterator[None]:
    global http
    reader = asyncio.StreamReader()
    transport, _ = await asyncio.get_running_loop().connect_read_pipe(
        lambda: asyncio.StreamReaderProtocol(reader), sys.stdin
    )

    async def commands() -> None:
        async for line in reader:
            gates[json.loads(line)["release"]].set()

    task = asyncio.create_task(commands())
    try:
        factory = peer_binding["http_client"] if peer_binding else http_client
        async with factory() as http:
            yield
    finally:
        task.cancel()
        transport.close()
        with suppress(asyncio.CancelledError):
            await task


server = MCPServer("ordinary-app", lifespan=lifespan)


async def call_peer(name: str) -> CallToolResult:
    transport = streamable_http_client(peer_binding["endpoint"], http_client=http)
    async with Client(transport, mode="2026-07-28") as client:
        return await client.call_tool(name, {})


@server.tool()
async def external() -> CallToolResult:
    response = await http.get(
        os.environ["EXTERNAL_URL"],
        headers={
            "x-holon-context": "must-not-leak",
            "x-holon-other": "must-not-leak",
        },
    )
    return CallToolResult(content=[TextContent(type="text", text=response.text)])


@server.tool()
async def credentials() -> CallToolResult:
    await http.get(
        os.environ["PEER_MCP_URL"], headers={"authorization": "Bearer app-token"}
    )
    return CallToolResult(content=[])


@server.tool()
async def redirect() -> CallToolResult:
    await http.get(os.environ["PEER_MCP_URL"])
    return CallToolResult(content=[])


@server.tool()
async def wait() -> CallToolResult:
    if "PEER_MCP_URL" in os.environ:
        return await call_peer("wait")
    emit("waiting")
    try:
        await asyncio.Event().wait()
    finally:
        emit("cancelled")
    return CallToolResult(content=[])


@server.tool()
async def inspect(ctx: Context[Any, Any]) -> CallToolResult:
    if "PEER_MCP_URL" in os.environ:
        return await call_peer("inspect")
    if os.environ.get("TEST_INSPECT_BARRIER"):
        emit("inspecting")
        await gates["inspect"].wait()
    token = get_access_token()
    identity = None
    if token is not None:
        identity = {
            "token": token.token,
            "extra": token.claims,
            "scopes": token.scopes,
            "clientId": token.client_id,
            "expiresAt": token.expires_at,
            "resource": token.resource,
        }
    return CallToolResult(
        content=[
            TextContent(
                type="text",
                text=json.dumps(
                    {"identity": identity, "headers": list(ctx.headers or {})}
                ),
            )
        ]
    )


create_app = server.streamable_http_app


def make_app(**options: Any) -> ASGIApp:
    handler = create_app(**options)

    async def app(scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] == "lifespan":

            async def ready(message: Message) -> None:
                await send(message)
                if message["type"] == "lifespan.startup.complete":
                    emit("ready")

            await handler(scope, receive, ready)
            return
        stream = Headers(scope=scope).get("x-test-stream")
        if stream == "peer":

            async def peer_body() -> AsyncIterator[bytes]:
                try:
                    yield b"peer stream"
                    await asyncio.Event().wait()
                finally:
                    emit("peer-stream-closed")

            await StreamingResponse(peer_body())(scope, receive, send)
            return
        if stream == "source":
            http = http_client()
            peer = await http.send(
                http.build_request(
                    "POST",
                    os.environ["PEER_MCP_URL"],
                    headers={"x-test-stream": "peer"},
                ),
                stream=True,
            )
            emit("stream-peer-ready")

            async def read_peer() -> None:
                try:
                    await peer.aread()
                except httpx2.HTTPError:
                    pass
                finally:
                    await peer.aclose()
                await http.aclose()
                block = (await call_peer("inspect")).content[0]
                assert isinstance(block, TextContent)
                emit(
                    "stream-late-forwarded", identity=json.loads(block.text)["identity"]
                )

            asyncio.create_task(read_peer())

            async def source_body() -> AsyncIterator[bytes]:
                yield b"first chunk"
                await gates["stream"].wait()
                yield b"last chunk"

            await StreamingResponse(source_body())(scope, receive, send)
            return
        await handler(scope, receive, send)

    return app


server.streamable_http_app = make_app  # type: ignore[method-assign, assignment]


frontend = (
    Path(unquote(urlsplit(os.environ["TEST_FRONTEND"]).path))
    if "TEST_FRONTEND" in os.environ
    else ""
)
