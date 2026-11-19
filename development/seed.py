"""Arrange the donated language toolchains into independently copyable production roots."""
import hashlib
import json
import shutil
import subprocess
import tempfile
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
        spec = f"{manager}@{ver}"
        if spec not in managers:
            # npm pack verifies the registry's integrity. Install the identical archive used for the keys.
            with tempfile.TemporaryDirectory() as scratch:
                pack = ["npm", "pack", "--json", "--ignore-scripts", spec]
                packed, = json.loads(subprocess.check_output(pack, cwd=scratch))
                tarball = Path(scratch) / packed["filename"]
                install = ["npm", "install", "--global", "--prefix", worker / base.strip("/")]
                subprocess.run([*install, "--ignore-scripts", "--no-audit", "--no-fund", tarball], check=True)
                data = tarball.read_bytes()
            managers[spec] = base + "/bin"
            for algorithm in ("sha224", "sha256", "sha384", "sha512"):
                digest = hashlib.new(algorithm, data).hexdigest()
                managers[f"{spec}+{algorithm}.{digest}"] = managers[spec]
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
