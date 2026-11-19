"""Build one OCI index containing native development workers and their production bases."""
import gzip
import hashlib
import io
import json
import shutil
import subprocess
import sys
import tarfile
import tempfile
from collections import defaultdict
from pathlib import Path

OCI = "application/vnd.oci.image."


def encoded(value):
    return json.dumps(value, separators=(",", ":"), sort_keys=True).encode()


def descriptor(kind, path):
    with path.open("rb") as f:
        digest = hashlib.file_digest(f, "sha256").hexdigest()
    return {"mediaType": OCI + kind, "digest": "sha256:" + digest, "size": path.stat().st_size}


def blob(layout, desc):
    algorithm, digest = desc["digest"].split(":")
    return layout / "blobs" / algorithm / digest


def save(layout, desc, path):
    target = blob(layout, desc)
    target.parent.mkdir(parents=True, exist_ok=True)
    path.replace(target)
    return desc


def document(layout, kind, value):
    raw = encoded(value)
    desc = {"mediaType": OCI + kind, "digest": "sha256:" + hashlib.sha256(raw).hexdigest(), "size": len(raw)}
    target = blob(layout, desc)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(raw)
    return desc


def layer(layout, files):
    """Each entry is a native filesystem path, a symlink target, or file bytes."""
    def stable(info):
        info.uid = info.gid = info.mtime = 0
        info.uname = info.gname = ""
        return info

    with tempfile.TemporaryDirectory(dir=layout) as tmp:
        raw, compressed = (Path(tmp) / n for n in ("layer.tar", "layer.gz"))
        with tarfile.open(raw, "w", format=tarfile.GNU_FORMAT) as tar:
            for dest, source in sorted(files.items()):
                dest = dest.lstrip("/")
                if isinstance(source, Path):
                    tar.add(source, arcname=dest, filter=stable)
                else:
                    entry = tarfile.TarInfo(dest)
                    if isinstance(source, str):
                        entry.type, entry.linkname, entry.mode = tarfile.SYMTYPE, source, 0o777
                        tar.addfile(entry)
                    else:
                        entry.size, entry.mode = len(source), 0o444
                        tar.addfile(entry, io.BytesIO(source))
        with compressed.open("wb") as output, raw.open("rb") as data:
            with gzip.GzipFile(filename="", fileobj=output, mode="wb", compresslevel=1, mtime=0) as gz:
                shutil.copyfileobj(data, gz)
            data.seek(0)
            diff = "sha256:" + hashlib.file_digest(data, "sha256").hexdigest()
        return {"blob": save(layout, descriptor("layer.v1.tar+gzip", compressed), compressed), "diff": diff}


def seed(out):
    out.mkdir(parents=True, exist_ok=True)
    roles, managers = {}, {}
    # /worker contains the supervisor and resolvers copied from pinned build stages.
    worker = {str(p.relative_to("/worker")): p for p in Path("/worker").rglob("*") if p.is_file()}
    for prefix in [Path("/usr/local"), *sorted(Path("/tmp/latest/node-versions").glob("v*/installation"))]:
        node = prefix / "bin/node"
        version = prefix.parent.name
        if prefix == Path("/usr/local"):
            version = subprocess.check_output([node, "--version"], text=True).strip()
        path = "/opt/holon/node/versions/" + version + "/installation"
        roles["node-" + version] = dict(path=path, layer=layer(out, {path + "/bin/node": node}))
        for package in sorted((prefix / "lib/node_modules").glob("*/package.json")):
            pkg = json.loads(package.read_bytes())
            if pkg["name"] not in ("npm", "pnpm"):
                continue
            name, ver = pkg["name"], pkg["version"]
            base = f"/opt/holon/managers/{name}/{ver}"
            managers[name + "@" + ver] = base + "/bin"
            worker[base + "/lib/" + name] = package.parent
            for command, target in pkg["bin"].items():
                worker[base + "/bin/" + command] = "../lib/" + name + "/" + target
                worker[path + "/bin/" + command] = base + "/bin/" + command
                if prefix == Path("/usr/local"):
                    worker["/usr/local/bin/" + command] = base + "/bin/" + command
        if prefix == Path("/usr/local"):
            worker["/usr/local/bin/node"] = path + "/bin/node"

    for python in sorted(Path("/opt/uv/python").glob("cpython-*")):
        if python.is_symlink() or not python.is_dir():
            continue
        version = python.name.split("-")[1]
        minor = "python" + ".".join(version.split(".")[:2])
        interpreter = str(python / "bin" / minor)
        files = {str(python): python, "/usr/local/bin/" + minor: interpreter}
        roles["python-" + version] = dict(path=str(python), layer=layer(out, files))
    (out / "seeds.json").write_bytes(encoded(dict(roles=roles, managers=managers, worker=layer(out, worker))))


def pack(layout, inputs):
    def index(children):
        return document(layout, "index.v1+json", dict(schemaVersion=2, mediaType=OCI+"index.v1+json", manifests=children))

    def load(desc):
        return json.loads(blob(layout, desc).read_bytes())

    def natives(manifests):
        for desc in manifests:
            if desc["mediaType"] == OCI + "index.v1+json":
                yield from natives(load(desc)["manifests"])
            elif desc.get("platform", {}).get("os") == "linux":
                yield desc

    def image(base, parts, node_path=None, worker=False):
        manifest = load(base)
        config = load(manifest["config"])
        for part in parts:
            manifest["layers"].append(part["blob"])
            config["rootfs"]["diff_ids"].append(part["diff"])
            config.setdefault("history", []).append({"created_by": "Holon runtime package"})
        if node_path:
            env = config["config"]["Env"]
            env[:] = [s.replace(".toolchain/node/bin:", ".toolchain/node/bin:" + node_path + "/bin:", 1) if s.startswith("PATH=") else s for s in env]
        if worker:
            config["config"]["Entrypoint"] = ["/holon-mcp-runtime"]
        manifest["config"] = document(layout, "config.v1+json", config)
        return dict(document(layout, "manifest.v1+json", manifest), platform=base["platform"])

    archs, roles = [], defaultdict(list)
    for base in natives(json.loads((layout / "index.json").read_bytes())["manifests"]):
        platform = base["platform"]
        stage = inputs / (platform["os"] + "_" + platform["architecture"])
        if not stage.exists():
            stage = inputs  # BuildKit omits the platform folder on a single-platform build.
        data = json.loads((stage / "seeds.json").read_bytes())
        for part in [data["worker"], *(r["layer"] for r in data["roles"].values())]:
            desc = part["blob"]
            target = blob(layout, desc)
            if not target.exists():
                shutil.copyfile(blob(stage, desc), target)
        archs.append((base, data))
        roles["core"].append(base)
        for name, role in data["roles"].items():
            roles[name].append(image(base, [role["layer"]], role["path"] if name.startswith("node-") else None))
    if not archs or any(len(children) != len(archs) for children in roles.values()):
        raise ValueError("the native builds must provide the same release toolchains")
    indexes = {name: index(children) for name, children in roles.items()}
    workers = []
    for base, data in archs:
        bases = {r["path"]: indexes[name]["digest"] for name, r in data["roles"].items()}
        bases["core"] = indexes["core"]["digest"]
        catalog = encoded(dict(bases=bases, managers=data["managers"]))
        parts = [r["layer"] for r in data["roles"].values()] + [data["worker"], layer(layout, {"etc/holon-runtime.json": catalog})]
        workers.append(image(base, parts, worker=True))
    siblings = [dict(d, platform=dict(os="unknown", architecture="unknown"), annotations={"sh.holon.production-base": n}) for n, d in indexes.items()]
    root = index(workers + siblings)
    tagged = dict(root, annotations={"org.opencontainers.image.ref.name": "release"})
    (layout / "index.json").write_bytes(encoded(dict(schemaVersion=2, mediaType=OCI+"index.v1+json", manifests=[tagged])))
    print(root["digest"])


if __name__ == "__main__":
    {"seed": seed, "pack": pack}[sys.argv[1]](*map(Path, sys.argv[2:]))
