import json
import os
import platform
from pathlib import Path

from mcp.server import MCPServer

server = MCPServer("resolver-check")
frontend = Path("/tmp")
(frontend / "runtime.json").write_text(json.dumps({
    "marker": "first", "version": platform.python_version(), "uid": os.getuid(),
}))
