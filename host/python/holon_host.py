import json
import math
import os
import re
import socket
import sys
import traceback
from contextvars import ContextVar
from typing import Any
from urllib.parse import quote

import httpcore2
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

try:
    from opentelemetry import context as tracing
    from opentelemetry import propagate
except ImportError:  # without OpenTelemetry the MCP SDK has no span to parent
    tracing = None  # type: ignore[assignment]

_scope: ContextVar[str] = ContextVar("holon_scope", default="")


def contain_http(host: str) -> None:
    broker = re.compile(re.escape(host) + r"/peer/[^/?#]+")
    send = httpx2.AsyncClient.send

    async def send_in_scope(
        client: httpx2.AsyncClient, request: httpx2.Request, **options: Any
    ) -> httpx2.Response:
        if not broker.fullmatch(str(request.url)):
            for name in list(request.headers):
                if name.startswith("x-holon-"):
                    del request.headers[name]
            return await send(client, request, **options)
        request.headers["x-holon-scope"] = _scope.get()
        response = await send(client, request, **options)
        if "x-holon-error" in response.headers:
            await response.aclose()
            raise RuntimeError("Holon peer call rejected or cancelled")
        return response

    httpx2.AsyncClient.send = send_in_scope  # type: ignore[method-assign, assignment]


async def start(entry: str) -> None:
    host = os.environ["HOLON_HOST_URL"]
    sys.path.insert(0, os.getcwd())
    contain_http(host)
    try:
        __import__(entry)
    except Exception as error:
        # Unlike importlib.import_module, the import statement drops importlib's frames, so skipping this one
        # leaves only the application's.
        assert error.__traceback__ is not None
        traceback.print_exception(error.with_traceback(error.__traceback__.tb_next))
        raise SystemExit(1) from error
    application = sys.modules[entry]
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
        async with httpcore2.AsyncConnectionPool() as control:

            async def dispatch(scope: Scope, receive: Receive, send: Send) -> None:
                if scope["type"] == "lifespan":

                    async def startup(message: Message) -> None:
                        if message["type"] == "lifespan.startup.complete":
                            response = await control.request(
                                "POST",
                                f"{host}/ready",
                                headers={"content-type": "application/json"},
                                content=json.dumps(
                                    {
                                        "port": listener.getsockname()[1],
                                        "frontend": frontend,
                                    }
                                ).encode(),
                            )
                            if response.status != 204:
                                raise RuntimeError(
                                    "Holon host rejected application startup"
                                )
                        await send(message)

                    await app(scope, receive, startup)
                    return
                identifier = Headers(scope=scope).get("x-holon-scope", "")
                response = await control.request(
                    "GET", f"{host}/context/{quote(identifier, safe='')}"
                )
                if response.status != 200:
                    await Response("Request context unavailable", status_code=403)(
                        scope, receive, send
                    )
                    return
                identity = json.loads(response.content)
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
                # The SDK's server span parents on params._meta, else on this context: the caller's trace headers.
                trace = (
                    tracing.attach(propagate.extract(Headers(scope=scope)))
                    if tracing
                    else None
                )
                try:
                    await app(scope, receive, send)
                finally:
                    if trace is not None:
                        tracing.detach(trace)
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
