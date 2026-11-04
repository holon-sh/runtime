import importlib
import math
import os
import re
import socket
import sys
from contextvars import ContextVar
from urllib.parse import quote

import httpx2
import uvicorn
from mcp.server.auth.middleware.auth_context import AuthContextMiddleware
from mcp.server.auth.middleware.bearer_auth import AuthenticatedUser
from mcp.server.auth.provider import AccessToken
from mcp.server.transport_security import TransportSecuritySettings
from starlette.authentication import AuthCredentials, UnauthenticatedUser
from starlette.datastructures import Headers
from starlette.responses import Response
from starlette.types import Message, Receive, Scope, Send

_scope: ContextVar[str] = ContextVar("holon_scope", default="")


def http_client() -> httpx2.AsyncClient:
    peer = re.compile(re.escape(os.environ["HOLON_HOST_URL"]) + r"/peer/[^/?#]+")

    async def prepare(request: httpx2.Request) -> None:
        if not peer.fullmatch(str(request.url)):
            for name in list(request.headers):
                if name.startswith("x-holon-"):
                    del request.headers[name]
            return
        request.headers = httpx2.Headers(
            [*request.headers.raw, (b"x-holon-scope", _scope.get().encode())]
        )

    async def check(response: httpx2.Response) -> None:
        if (
            peer.fullmatch(str(response.request.url))
            and "x-holon-error" in response.headers
        ):
            await response.aclose()
            raise RuntimeError("Holon peer call rejected or cancelled")

    return httpx2.AsyncClient(
        timeout=httpx2.Timeout(30.0, read=300.0),
        trust_env=False,
        event_hooks={"request": [prepare], "response": [check]},
    )


def binding(app: str) -> dict[str, object] | None:
    endpoint = os.environ.get(app.upper().replace("-", "_") + "_MCP_URL")
    return {"endpoint": endpoint, "http_client": http_client} if endpoint else None


async def start(entry: str) -> None:
    host = os.environ["HOLON_HOST_URL"]
    sys.path.insert(0, os.getcwd())
    try:
        from holon.binding import set_default_binding
    except ModuleNotFoundError as error:
        if error.name not in {"holon", "holon.binding"}:
            raise
    else:
        set_default_binding(binding)
    application = importlib.import_module(entry)
    app = AuthContextMiddleware(
        application.server.streamable_http_app(
            stateless_http=True,
            transport_security=TransportSecuritySettings(
                enable_dns_rebinding_protection=False
            ),
        )
    )
    frontend = os.fspath(getattr(application, "frontend", ""))
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        async with httpx2.AsyncClient(trust_env=False) as control:

            async def dispatch(scope: Scope, receive: Receive, send: Send) -> None:
                if scope["type"] == "lifespan":

                    async def startup(message: Message) -> None:
                        if message["type"] == "lifespan.startup.complete":
                            response = await control.post(
                                f"{host}/ready",
                                json={
                                    "port": listener.getsockname()[1],
                                    "frontend": frontend,
                                },
                            )
                            response.raise_for_status()
                        await send(message)

                    await app(scope, receive, startup)
                    return
                identifier = Headers(scope=scope).get("x-holon-scope", "")
                response = await control.get(
                    f"{host}/context/{quote(identifier, safe='')}"
                )
                if not response.is_success:
                    await Response("Request context unavailable", status_code=403)(
                        scope, receive, send
                    )
                    return
                identity = response.json()
                scope["headers"] = [
                    (name, value)
                    for name, value in scope["headers"]
                    if name != b"x-holon-scope"
                ]
                scope["user"] = UnauthenticatedUser()
                scope["auth"] = AuthCredentials()
                if identity is not None:
                    token = AccessToken(
                        token=identity["token"],
                        client_id=identity["clientId"],
                        scopes=identity["scopes"],
                        expires_at=math.floor(identity["expiresAt"]),
                        resource=identity["resource"],
                        subject=identity["extra"].get("sub"),
                        claims=identity["extra"],
                    )
                    scope["user"] = AuthenticatedUser(token)
                    scope["auth"] = AuthCredentials(token.scopes)
                current = _scope.set(identifier)
                try:
                    await app(scope, receive, send)
                finally:
                    _scope.reset(current)

            await uvicorn.Server(
                uvicorn.Config(
                    dispatch,
                    lifespan="on",
                    access_log=False,
                    log_level="warning",
                    proxy_headers=False,
                    ws="none",
                    timeout_graceful_shutdown=1,
                )
            ).serve(sockets=[listener])
