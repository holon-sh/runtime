from collections.abc import Callable
from typing import Any

_resolver: Callable[[str], Any] | None = None


def set_default_binding(resolve: Callable[[str], Any]) -> None:
    global _resolver
    _resolver = resolve


def resolve_default_binding(app: str) -> Any:
    return _resolver(app) if _resolver is not None else None
