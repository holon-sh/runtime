"""Package the multi-platform worker and its independently runnable production bases."""
import argparse
import json
import subprocess
import urllib.request
from pathlib import Path

from check_bytecode import check_bytecode

supported = ["linux/amd64", "linux/arm64"]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("output", type=Path)
parser.add_argument("--platform", choices=supported)
args = parser.parse_args()
out = args.output.resolve()
out.mkdir(parents=True, exist_ok=True)
platforms = [args.platform] if args.platform else supported
dockerfile = Path("development/Dockerfile").read_text()
index_type = "application/vnd.oci.image.index.v1+json"
# Give both native builds one exact LTS, even if a release happens during packaging.
with urllib.request.urlopen("https://nodejs.org/dist/index.json", timeout=30) as releases:
    node_lts = next(r["version"] for r in json.load(releases) if r["lts"])


def bake(targets):
    # Bake the same generated Dockerfile for both native platforms.
    generated = out / "roles.Dockerfile"
    generated.write_text(dockerfile)
    common = {
        "context": ".", "platforms": platforms, "dockerfile": str(generated),
        "args": {"NODE_LTS": node_lts, "SOURCE_DATE_EPOCH": "0"},
    }
    spec = {"target": {name: common | target for name, target in targets.items()},
            "group": {"default": {"targets": list(targets)}}}
    subprocess.run(
        ["docker", "buildx", "bake", "-f", "-",
         "--allow=fs.write=" + str(out), "--allow=fs.read=" + str(out)],
        input=json.dumps(spec), text=True, check=True,
    )


def oras(*args, data=None):
    return subprocess.run(["oras", *map(str, args)], input=data, text=True, check=True, stdout=subprocess.PIPE).stdout


def copy_layout(name):
    layout = out / name
    desc, = json.loads((layout / "index.json").read_bytes())["manifests"]
    target = str(out / "release") + ":" + name
    oras("cp", "--from-oci-layout", "--to-oci-layout", str(layout) + "@" + desc["digest"], target)
    if name.startswith("python-"):
        index = json.loads(oras("manifest", "fetch", "--oci-layout", target))
        for native in index["manifests"]:
            if native["platform"]["os"] != "linux":
                continue
            ref = str(out / "release") + "@" + native["digest"]
            manifest = json.loads(oras("manifest", "fetch", "--oci-layout", ref))
            check_bytecode(out / "release", manifest["layers"][-1])
    return json.loads(oras("manifest", "fetch", "--descriptor", "--oci-layout", target))


def image(name):
    options = "type=oci,tar=false,oci-mediatypes=true,compression=zstd,compression-level=15"
    return {"target": name, "attest": ["type=provenance,mode=min"], "output": [options + ",dest=" + str(out / name)]}


def copy_role(name):
    return f"COPY --link --from=seed /out/{name}/ /\n"


discovery = out / "discovery"
bake({"discovery": {"target": "discovery", "output": ["type=local,dest=" + str(discovery)]}})
native = {}
for platform in platforms:
    directory = discovery / platform.replace("/", "_")
    directory.mkdir(exist_ok=True)
    seeds = directory / "seeds.json"
    if len(platforms) == 1 and not seeds.exists():
        seeds = discovery / "seeds.json"
    native[directory] = json.loads(seeds.read_bytes())
roles = sorted(next(iter(native.values()))["roles"])
if any(sorted(d["roles"]) != roles for d in native.values()):
    raise ValueError("all requested platforms must supply the same toolchain releases")
for role in roles:
    dockerfile += f"\nFROM core AS {role}\n" + copy_role(role)
bake({name.replace(".", "-"): image(name) for name in ["core", *roles]})
siblings = {name: copy_layout(name) for name in ["core", *roles]}
for path, data in native.items():
    bases = {directory: siblings[role]["digest"] for role, directory in data["roles"].items()}
    bases["core"] = siblings["core"]["digest"]
    (path / "catalog.json").write_text(json.dumps(dict(bases=bases, managers=data["managers"]), sort_keys=True))

dockerfile += "\nFROM core AS release-worker\n" + "".join(map(copy_role, [*roles, "worker"]))
dockerfile += 'ARG TARGETARCH\nCOPY --from=catalog /linux_${TARGETARCH}/catalog.json /etc/holon-runtime.json\nENTRYPOINT ["/holon-mcp-runtime"]\n'
bake({"release-worker": {**image("release-worker"), "contexts": {"catalog": str(discovery)}}})
worker = copy_layout("release-worker")
release = str(out / "release")
root = json.loads(oras("manifest", "fetch", "--oci-layout", release + "@" + worker["digest"]))
# A normal pull selects the worker. ORAS also copies the bases reachable by digest.
for name, sibling in siblings.items():
    root["manifests"].append(sibling | {
        "platform": {"os": "unknown", "architecture": "unknown"},
        "annotations": {"sh.holon.production-base": name},
    })
oras("manifest", "push", "--oci-layout", release + ":release", "--media-type", index_type, "-", data=json.dumps(root, separators=(",", ":")))
print(oras("resolve", "--oci-layout", release + ":release").strip())
