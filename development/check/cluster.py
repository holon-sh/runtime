"""Run released CLI overlays in a disposable native Kubernetes cluster. Requires Docker buildx, k3d, kubectl, uv and Go."""

import contextlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
from urllib.error import HTTPError
from urllib.request import Request, urlopen

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
ARCH = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
REGISTRY = "registry:3@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c"
SQUID = "ubuntu/squid:6.10-24.10_beta@sha256:c9f5212b147a766529c7b026e2bebed37b998d33d0066b658596af5eba7cc65c"


def run(*args, data=None, env=None, cwd=None):
    command = subprocess.run(args, input=data, stdout=subprocess.PIPE, text=True, env=env, cwd=cwd, timeout=900)
    if command.returncode:
        raise RuntimeError(f"{command.args} exited {command.returncode}\n{command.stdout}")
    return command.stdout.strip()


def request(url, *, data=None, headers=None):
    try:
        with urlopen(Request(url, data=data, headers=headers or {}), timeout=450) as response:
            return json.load(response)
    except HTTPError as error:
        raise RuntimeError(f"{url} returned {error.code}: {error.read().decode()}") from error


@contextlib.contextmanager
def port_forward(pod):
    with subprocess.Popen(["kubectl", "port-forward", "pod/" + pod, ":3000"],
                          stdout=subprocess.PIPE, text=True) as forward:
        try:
            line = forward.stdout.readline()
            match = re.search(r"127\.0\.0\.1:(\d+)", line)
            if not match:
                raise RuntimeError(f"port-forward failed: {line}")
            yield "http://127.0.0.1:" + match[1]
        finally:
            forward.terminate()


def apply(obj):
    run("kubectl", "apply", "-f", "-", data=json.dumps(obj))


def start_pod(name, image, variables, *, production=False):
    scratch = {"name": "tmp", "emptyDir": {"medium": "Memory", "sizeLimit": "64Mi"}}
    volumes = [scratch]
    mounts = [{"name": "tmp", "mountPath": "/tmp"}]
    if not production:
        volumes.extend([{"name": "app", "emptyDir": {"sizeLimit": "2Gi"}},
                        {"name": "record", "configMap": {"name": name}}])
        mounts.extend([{"name": "app", "mountPath": "/app"}, {"name": "record", "mountPath": "/record"}])
    apply({"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "labels": {"mode": "offline" if production else "build"}},
           "spec": {"restartPolicy": "Never", "automountServiceAccountToken": False,
                    "securityContext": {"runAsUser": 10001, "runAsGroup": 10001, "fsGroup": 10001},
                    "volumes": volumes, "containers": [{"name": "app", "image": image, "imagePullPolicy": "Never",
                        "volumeMounts": mounts, "env": [{"name": k, "value": v} for k, v in variables.items()],
                        "securityContext": {"readOnlyRootFilesystem": True, "allowPrivilegeEscalation": False,
                                            "capabilities": {"drop": ["ALL"]}},
                        "readinessProbe": {"tcpSocket": {"port": 3000}, "periodSeconds": 1}}]}})
    try:
        run("kubectl", "wait", "--for=condition=Ready", "--timeout=120s", "pod/"+name)
    except RuntimeError:
        print(run("kubectl", "describe", "pod", name), flush=True)
        print(run("kubectl", "logs", name), flush=True)
        raise


def policy(mode, allowed):
    rules = [{"to": [{"ipBlock": {"cidr": ip + "/32"}}], "ports": [{"port": port}]} for ip, port in allowed]
    apply({"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": {"name": mode},
           "spec": {"podSelector": {"matchLabels": {"mode": mode}}, "policyTypes": ["Egress"], "egress": rules}})


def proof(cli, baseline, work, stack):
    name = "source-" + work.name[-8:].lower().replace("_", "x")
    node = f"k3d-{name}-server-0"
    binary = work / "released-cli.test"
    overlay = work / "source.tar.zst"
    go_overlay = work / "go-overlay.json"
    virtual = cli / "pkg/installation/source_image_test.go"
    go_overlay.write_text(json.dumps({"Replace": {str(virtual): str(HERE / "cli_test.go.txt")}}))
    run("go", "-C", cli, "test", "-c", "-overlay", go_overlay, "-o", binary, "./pkg/installation")
    # A private kubeconfig keeps all kubectl operations on this ephemeral cluster.
    os.environ["KUBECONFIG"] = str(work/"kubeconfig")
    stack.callback(run, "k3d", "cluster", "delete", name)
    run("k3d", "cluster", "create", name, "--image", "rancher/k3s:v1.36.4-k3s1", "--wait", "--no-lb",
        "--kubeconfig-update-default=false", "--kubeconfig-switch-context=false",
        "--k3s-arg", "--disable=traefik,servicelb,metrics-server,local-storage@server:0")
    (work/"kubeconfig").write_text(run("k3d", "kubeconfig", "get", name) + "\n")
    assert json.loads(run("kubectl", "get", "nodes", "-o", "json"))["items"][0]["status"]["nodeInfo"]["architecture"] == ARCH

    def service(role, image, *flags):
        container = name + "-" + role
        run("docker", "create", "--network", "k3d-"+name, "--name", container, *flags, image)
        stack.callback(run, "docker", "rm", "-f", container)
        if role == "proxy":
            run("docker", "cp", HERE/"squid.conf", container+":/etc/squid/squid.conf")
        run("docker", "start", container)
        return run("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", container)

    registry = service("registry", REGISTRY, "-p", "127.0.0.1::5000")
    proxy = service("proxy", SQUID)
    host = run("docker", "port", name+"-registry", "5000/tcp")
    policy("build", [(registry, 5000), (proxy, 3128)])
    policy("offline", [])

    def released(**variables):
        run(binary, "-test.run=^TestSourceImageContract$", env={**os.environ, "REGISTRY": host, "BASE_REPOSITORY": "base",
            "OVERLAY": str(overlay), **{k: str(v) for k, v in variables.items()}})

    def direct_production(sample, expected):
        # Measure the application separately from the kubelet's one-second probe.
        ref = host+"/"+sample+":production"
        run("docker", "pull", ref)
        started = time.monotonic()
        cid = run("docker", "run", "-d", "--read-only", "--user", "10001:10001",
                  "--cap-drop", "ALL", "--network", "none", "--tmpfs", "/tmp:rw,exec,size=64m", ref)
        try:
            script = 'until test -s /tmp/runtime.json; do sleep 0.02; done; cat /tmp/runtime.json'
            answer = json.loads(run("docker", "exec", cid, "timeout", "30", "sh", "-ec", script))
            assert (answer["uid"], answer["version"], answer["marker"]) == (10001, expected, "saved"), answer
            return time.monotonic()-started
        finally:
            run("docker", "rm", "-f", cid)

    def cache_image(ref):
        # The node imports the image. The app's network policy applies from its first instruction.
        ref = f"{registry}:5000/{ref}"
        run("docker", "exec", node, "ctr", "-n", "k8s.io", "images", "pull", "--plain-http", "--platform", "linux/"+ARCH, ref)
        return ref

    def build_base(source, label):
        archive = work/(label+".tar")
        if label == "base":
            archive = Path(os.environ["ROLE_ARCHIVE"])
        else:
            run("docker", "buildx", "build", "--platform", "linux/"+ARCH, "--provenance=true", "-t", "proof:base",
                "--output", f"type=oci,dest={archive}", "-f", source/"development/Dockerfile", source)
        with tarfile.open(archive) as tar:
            descriptors = json.load(tar.extractfile("index.json"))["manifests"]
            if label == "base":
                descriptors = [d for d in descriptors if d.get("annotations", {}).get("org.opencontainers.image.ref.name") == "release"]
            descriptor, = descriptors
            assert descriptor["mediaType"] == "application/vnd.oci.image.index.v1+json", descriptor
            base_digest = descriptor["digest"]
            def blob(digest):
                return json.load(tar.extractfile("blobs/" + digest.replace(":", "/")))
            native, = [m for m in blob(base_digest)["manifests"] if m.get("platform", {}).get("architecture") == ARCH]
            print(f"{ARCH} {label} compressed base layer bytes {sum(layer['size'] for layer in blob(native['digest'])['layers'])}", flush=True)
        released(BASE_OCI=archive, BASE_DIGEST=base_digest, BASE_REPOSITORY=label)
        return cache_image(label+"@"+base_digest)

    base = build_base(ROOT, "base")
    old_base = build_base(baseline, "oldbase")
    def build_pod(pod, app, image=base):
        record = json.dumps({"registry": f"http://{registry}:5000", "repository": app, "tag": "session", "token": "local"})
        run("kubectl", "create", "configmap", pod, "--from-literal=overlay.json="+record)
        proxy_url = f"http://{proxy}:3128"
        creating = time.monotonic()
        start_pod(pod, image, {"HOLON_BASE_IMAGE": image, "HOLON_OVERLAY_FILE": "/record/overlay.json",
                  "HTTP_PROXY": proxy_url, "HTTPS_PROXY": proxy_url, "http_proxy": proxy_url,
                  "https_proxy": proxy_url, "NO_PROXY": registry, "no_proxy": registry})
        pod_ready = time.monotonic()-creating
        status = run("kubectl", "exec", pod, "--", "timeout", "10", "bash", "-c",
                     'exec 3<>/dev/tcp/$1/3128; printf "CONNECT nodejs.org:443 HTTP/1.1\\r\\nHost: nodejs.org:443\\r\\n\\r\\n" >&3; head -1 <&3', "check-proxy", proxy)
        assert "200" in status, status
        return pod_ready

    def submit(url, project, entry, phase, *, build=False):
        with tempfile.TemporaryDirectory(dir=work) as staged:
            root = Path(staged)/"project"
            shutil.copytree(project, root)
            released(PROJECT=root, ENTRY=entry, BUILD=int(build))
        headers = {"Content-Type": "application/vnd.holon.source-overlay.v0.tar+zstd", "X-Holon-Build-Token": "local"}
        body = overlay.read_bytes()
        started = time.monotonic()
        reply = request(url+"/.holon/"+("build" if build else "reload"), data=body, headers=headers)
        seconds = time.monotonic()-started
        print(f"{ARCH} {phase} overlay POST-to-ready: {seconds:.3f}s", flush=True)
        return reply, seconds

    def check_response(url, version, marker, *, production=False):
        body = request(url+"/runtime.json")
        assert body["version"].startswith(version) and body["marker"] == marker and body["uid"] == 10001, body
        if "installer" in body:
            assert body["installer"] == (None if production else "pnpm@9.15.9"), body
        print(f"{ARCH} app answered {body}", flush=True)
        return body["version"]

    def source_edits(url, project, entry, phase, resolved):
        initial = (project/entry).read_text()
        samples = []
        for edit in range(1, 4):
            marker = f"edit-{edit}"
            changed = time.monotonic()
            (project/entry).write_text(initial.replace('"first"', json.dumps(marker)))
            reply, elapsed = submit(url, project, entry, phase+" "+marker)
            assert not reply.get("output"), reply
            check_response(url, resolved, marker)
            print(f"{ARCH} {phase} {marker} file-write to observed response {time.monotonic()-changed:.3f}s", flush=True)
            samples.append(elapsed)
        print(f"{ARCH} {phase} seconds {samples}", flush=True)
        return samples

    timings = {}
    for app, entry, pin, first, second in [
        ("node", "server.js", ".nvmrc", "22", "24"), ("python", "server.py", ".python-version", "3.12", "3.13"),
    ]:
        project = work/app
        project.mkdir()
        shutil.copyfile(HERE/entry, project/entry)
        (project/pin).write_text(first+"\n")
        if app == "node":
            sdk = json.loads((cli/"tooling/typescript/package.json").read_text())["devDependencies"]["holon-sh"]
            manifest = {"name": "check", "type": "module", "packageManager": "pnpm@9.15.9",
                        "dependencies": {"holon-sh": sdk}, "engines": {"node": "22"}}
            (project/"package.json").write_text(json.dumps(manifest))
            run("npm", "exec", "--yes", "--package=pnpm@9.15.9", "--", "pnpm", "install", "--lockfile-only", cwd=project)
        else:
            (project/"pyproject.toml").write_text('[project]\nname="check"\nversion="0.0.0"\nrequires-python=">=3.12"\ndependencies=["holon-sh==0.0.1.dev26"]\n')
            run("uv", "lock", "--directory", project, "--default-index", "https://pypi.org/simple")

        if app == "python":
            control = work/"published"
            shutil.copytree(project, control)
            build_pod("python-published", app, old_base)
            with port_forward("python-published") as url:
                submit(url, control, entry, "dev.20 rebuild Python cold")
                version = check_response(url, first+".", "first")
                source_edits(url, control, entry, "dev.20 rebuild Python source-only", version)
            run("kubectl", "delete", "pod", "python-published", "--wait=false")

        pod = app+"-dev"
        build_pod(pod, app)
        with port_forward(pod) as url:
            submit(url, project, entry, f"{app} cold")
            resolved = check_response(url, first+".", "first")
            timings[app] = source_edits(url, project, entry, app+" source-only", resolved)

            if app == "node":
                (project/pin).unlink()
                pin = ".node-version"
            (project/pin).write_text(second+"\n")
            submit(url, project, entry, f"{app} version change")
            check_response(url, second+".", "edit-3")
            if app == "node":
                (project/pin).unlink()
                submit(url, project, entry, "Node from package.json engines")
                check_response(url, "22.", "edit-3")
                del manifest["engines"]
                manifest["devEngines"] = {"runtime": [{"name": "node", "version": "^24"}]}
                (project/"package.json").write_text(json.dumps(manifest))
                submit(url, project, entry, "Node from package.json devEngines.runtime")
                check_response(url, "24.", "edit-3")
                del manifest["devEngines"]
                (project/"package.json").write_text(json.dumps(manifest))
                submit(url, project, entry, "Node without a version declaration")
                latest = next(v["version"][1:] for v in request("https://nodejs.org/dist/index.json") if v["lts"])
                check_response(url, latest, "edit-3")
        run("kubectl", "delete", "pod", pod, "--wait=false")
        (project/pin).write_text(first+"\n")

        images = []
        for repetition in range(2):
            pod = f"{app}-build-{repetition}"
            build_pod(pod, app)
            with port_forward(pod) as url:
                reply, _ = submit(url, project, entry, f"{app} fresh freeze {repetition+1}", build=True)
            images.append(reply["image"])
            run("kubectl", "delete", "pod", pod, "--wait=false")
        assert images[0] == images[1], f"fresh {app} freezes differ: {images}"
        manifest = request(f"http://{host}/v2/{app}/manifests/{images[0]}", headers={"Accept":"application/vnd.oci.image.manifest.v1+json"})
        assert len(manifest["layers"]) == 3, manifest
        print(f"{ARCH} identical {app} freezes {images[0]}, compressed layer bytes {[x['size'] for x in manifest['layers']]}", flush=True)

        released(FROZEN=images[0], APP=app, ENTRY=entry)
        frozen = cache_image(app+":production")
        pod = app+"-offline"
        start_pod(pod, frozen, {"HOST":"0.0.0.0"}, production=True)
        with port_forward(pod) as url:
            check_response(url, resolved, "edit-3", production=True)
        # The same live registry accepted both build pushes. Offline pods must fail to connect to it.
        if app == "node":
            probe = ["node", "-e", "const s=require('net').connect(5000,process.argv[1],()=>process.exit(1));s.on('error',()=>{});s.setTimeout(2500,()=>s.destroy())"]
        else:
            probe = [".venv/bin/python", "-c", "import socket,sys;s=socket.socket();s.settimeout(2.5);sys.exit(s.connect_ex((sys.argv[1],5000))==0)"]
        run("kubectl", "exec", pod, "--", *probe, registry)
        run("kubectl", "delete", "pod", pod, "--wait=false")

    # A Python app can consume Node in its page build without shipping it.
    hybrid = work/"hybrid"
    shutil.copytree(work/"python", hybrid)
    (hybrid/"package.json").write_text(json.dumps({"name": "page", "private": True, "engines": {"node": "22"},
        "dependencies": {"ms": "2.1.3"}, "scripts": {"build": "node page.cjs"}}))
    (hybrid/"page.cjs").write_text("const fs = require('fs'); fs.mkdirSync('dist',{recursive:true}); "
        "fs.writeFileSync('dist/page.json', JSON.stringify({ minute: require('ms')('1m'), node: process.version }));")
    (hybrid/"server.py").write_text("from pathlib import Path\nfrom mcp.server import MCPServer\n"
        "server = MCPServer('page')\nfrontend = Path(__file__).with_name('dist')\n")
    build_pod("hybrid-build", "hybrid")
    with port_forward("hybrid-build") as url:
        reply, _ = submit(url, hybrid, "server.py", "Python with Node page build", build=True)
    digest = reply["image"]
    run("kubectl", "delete", "pod", "hybrid-build", "--wait=false")
    released(FROZEN=digest, APP="hybrid", ENTRY="server.py")
    start_pod("hybrid-offline", cache_image("hybrid:production"), {"HOST": "0.0.0.0"}, production=True)
    with port_forward("hybrid-offline") as url:
        page = request(url+"/page.json")
        assert page["minute"] == 60000 and page["node"].startswith("v22."), page
    manifest = request(f"http://{host}/v2/hybrid/manifests/{digest}", headers={"Accept":"application/vnd.oci.image.manifest.v1+json"})
    names = set()
    for layer in manifest["layers"]:
        with urlopen(f"http://{host}/v2/hybrid/blobs/{layer['digest']}", timeout=240) as src, (work/"layer.zst").open("wb") as dest:
            shutil.copyfileobj(src, dest)
        names.update(n.rstrip("/") for n in run("tar", "-tf", work/"layer.zst").splitlines())
    assert {"app/source/.venv/bin/python", "app/source/dist/page.json"} <= names and any(
        n.startswith("app/source/.toolchain/python/") for n in names), "missing Python toolchain, venv or built page"
    assert not any(n == prefix or n.startswith(prefix+"/") for n in names
        for prefix in ["app/source/.toolchain/node", "app/source/node_modules"]), "Node present in a frozen layer"
    print(f"{ARCH} offline hybrid answered {page}; all {len(manifest['layers'])} frozen layers contain no Node installation", flush=True)
    # Both images run exactly the versions of the currently supported project. Reuse image layers on
    # the same node and registry across trials, but give each dev/build its own empty /app and /tmp.
    versions_pod = "baseline-versions"
    build_pod(versions_pod, "versions", old_base)
    def version(program, *args):
        return run("kubectl", "exec", versions_pod, "--", program, *args)
    node_version = version("node", "-p", "process.versions.node")
    manager_version = version("pnpm", "--version")
    python_version = version("python3.13", "-c", "import platform; print(platform.python_version())")
    run("kubectl", "delete", "pod", versions_pod, "--wait=false")
    print(f"BENCH {ARCH} versions node={node_version} pnpm={manager_version} python={python_version}", flush=True)

    def timed(fn, *args, **kwargs):
        started = time.monotonic()
        value = fn(*args, **kwargs)
        return value, time.monotonic()-started

    for app, entry, version_file, expected in [
        ("node", "server.js", ".nvmrc", node_version),
        ("python", "server.py", ".python-version", python_version),
    ]:
        project = work/("benchmark-"+app)
        project.mkdir()
        shutil.copyfile(HERE/entry, project/entry)
        (project/version_file).write_text(expected+"\n")
        if app == "node":
            sdk = json.loads((cli/"tooling/typescript/package.json").read_text())["devDependencies"]["holon-sh"]
            (project/"package.json").write_text(json.dumps({"name":"check", "type":"module",
                "packageManager":"pnpm@"+manager_version, "dependencies":{"holon-sh":sdk}}))
            run("npm", "exec", "--yes", "--package=pnpm@"+manager_version, "--", "pnpm", "install", "--lockfile-only", cwd=project)
        else:
            for filename in ["pyproject.toml", "uv.lock"]:
                shutil.copyfile(work/"python"/filename, project/filename)

        def observed(url, marker):
            answer = request(url+"/runtime.json")
            assert answer["version"] == expected and answer["marker"] == marker and answer["uid"] == 10001, answer

        for rep in range(5):
            variants = [("dev20", old_base, "oldbase"), ("candidate", base, "base")]
            if rep % 2:
                variants.reverse()
            for side, image, base_repository in variants:
                sample = f"{app}-{side}-{rep}"
                dev, build, production = (sample+"-"+phase for phase in ("dev", "build", "prod"))
                measurements = {}
                shutil.copyfile(HERE/entry, project/entry)
                measurements["dev_pod_ready"] = build_pod(dev, sample, image)
                with port_forward(dev) as url:
                    ((_, post), total) = timed(submit, url, project, entry, sample+" cold dev")
                    measurements.update(dev_cold_cli_and_ready=total, dev_cold_post=post)
                    observed(url, "first")
                    if app == "node":
                        actual = run("kubectl", "exec", dev, "--", "sh", "-c", "cd /app/source && pnpm --version")
                        assert actual == manager_version, (actual, manager_version)
                    started = time.monotonic()
                    (project/entry).write_text((project/entry).read_text().replace('"first"', '"saved"'))
                    _, post = submit(url, project, entry, sample+" save")
                    observed(url, "saved")
                    measurements.update(save_write_to_observed=time.monotonic()-started, save_post=post)
                run("kubectl", "delete", "pod", dev, "--wait=false")
                measurements["build_pod_ready"] = build_pod(build, sample, image)
                with port_forward(build) as url:
                    ((reply, post), total) = timed(submit, url, project, entry, sample+" deploy build", build=True)
                    measurements.update(deploy_cli_and_freeze=total, build_freeze_post=post)
                _, measurements["cli_assemble"] = timed(released, FROZEN=reply["image"], APP=sample, ENTRY=entry,
                                                         BASE_REPOSITORY=base_repository)
                media = {"Accept": "application/vnd.oci.image.manifest.v1+json"}
                manifests = f"http://{host}/v2/{sample}/manifests/"
                assembled = request(manifests+"production", headers=media)
                layers = request(manifests+reply["image"], headers=media)["layers"]
                sizes = {"base": sum(l["size"] for l in assembled["layers"][:-len(layers)]),
                         "frozen": [l["size"] for l in layers], "total": sum(l["size"] for l in assembled["layers"])}
                assert assembled["layers"][-len(layers):] == layers, "assembled image did not append the frozen layers"
                print("BYTES", ARCH, app, side, rep+1, json.dumps(sizes), flush=True)
                frozen, measurements["pull_assembled"] = timed(cache_image, sample+":production")
                began = time.monotonic()
                start_pod(production, frozen, {"HOST":"0.0.0.0"}, production=True)
                with port_forward(production) as url:
                    observed(url, "saved")
                measurements["production_ready_and_observed"] = time.monotonic()-began
                measurements["deploy_total"] = sum(measurements[k] for k in
                    ["build_pod_ready","deploy_cli_and_freeze","cli_assemble","pull_assembled","production_ready_and_observed"])
                if side == "candidate":
                    absent = ["/holon-mcp-runtime", "/usr/local/bin/fnm", "/usr/local/bin/uv", "/opt/holon/managers"]
                    absent.append("/opt/uv/python" if app == "node" else "/opt/holon/node")
                    run("kubectl", "exec", production, "--", "sh", "-ec",
                        'for file do test ! -e "$file"; done', "check-production", *absent)
                measurements["direct_offline_start_and_marker"] = direct_production(sample, expected)
                elapsed = {key: round(value, 3) for key, value in measurements.items()}
                print("BENCH", ARCH, app, side, rep+1, json.dumps(elapsed), flush=True)
                run("kubectl", "delete", "pod", build, production, "--wait=false")
    access = run("docker", "exec", name+"-proxy", "cat", "/var/log/squid/access.log")
    assert "TCP_DENIED" not in access and "nodejs.org:443" in access and "releases.astral.sh:443" in access, access
    slow = {app: samples for app, samples in timings.items() if max(samples) >= 1}
    assert not slow, f"source-only edits must finish in under a second: {slow}"


if __name__ == "__main__":
    with tempfile.TemporaryDirectory(prefix="source-image-") as tmp, contextlib.ExitStack() as resources:
        proof(Path(sys.argv[1]).resolve(), Path(sys.argv[2]).resolve(), Path(tmp), resources)
