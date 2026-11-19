"""Arrange the donated language toolchains into independently copyable production roots."""
import json
import shutil
import subprocess
from pathlib import Path

out = Path("/out")
worker = out / "worker"
shutil.copytree("/worker", worker, symlinks=True)
roles, managers = {}, {}


def link(root, name, target):
    dest = root / name.lstrip("/")
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.symlink_to(target)


for prefix in [Path("/usr/local"), *sorted(Path("/tmp/latest/node-versions").glob("v*/installation"))]:
    node = prefix / "bin/node"
    version = prefix.parent.name
    if prefix == Path("/usr/local"):
        version = subprocess.check_output([node, "--version"], text=True).strip()
    name = "node-" + version
    if name in roles:
        continue
    path = "/opt/holon/node/versions/" + version + "/installation"
    roles[name] = path
    target = out / name / path.strip("/") / "bin"
    target.mkdir(parents=True, exist_ok=True)
    shutil.copy2(node, target / "node")
    link(out / name, "/usr/local/bin/node", path + "/bin/node")
    for package in sorted((prefix / "lib/node_modules").glob("*/package.json")):
        pkg = json.loads(package.read_bytes())
        manager, ver = pkg["name"], pkg["version"]
        if manager not in ("npm", "pnpm"):
            continue
        base = f"/opt/holon/managers/{manager}/{ver}"
        managers[manager + "@" + ver] = base + "/bin"
        lib = worker / base.strip("/") / "lib" / manager
        if not lib.exists():
            shutil.copytree(package.parent, lib, symlinks=True)
            for cmd, script in pkg["bin"].items():
                link(worker, base + "/bin/" + cmd, "../lib/" + manager + "/" + script)
        for cmd in pkg["bin"]:
            link(worker, path + "/bin/" + cmd, base + "/bin/" + cmd)
            if prefix == Path("/usr/local"):
                link(worker, "/usr/local/bin/" + cmd, base + "/bin/" + cmd)
    if prefix == Path("/usr/local"):
        link(worker, "/usr/local/bin/node", path + "/bin/node")

for python in sorted(Path("/opt/uv/python").glob("cpython-*")):
    if python.is_symlink() or not python.is_dir():
        continue
    version = python.name.split("-")[1]
    name = "python-" + version
    minor = "python" + ".".join(version.split(".")[:2])
    roles[name] = str(python)
    shutil.copytree(python, out / name / str(python).strip("/"), symlinks=True)
    link(out / name, "/usr/local/bin/" + minor, str(python / "bin" / minor))

Path("/discovery").mkdir()
Path("/discovery/seeds.json").write_text(json.dumps(dict(roles=roles, managers=managers), sort_keys=True))
