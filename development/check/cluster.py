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
PUBLISHED_BASE = "ghcr.io/holon-sh/development@sha256:799756844538e51f867b2ecb0f82bd311a2090a8bf476c11141778509742975a"
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


def proof(cli, work, stack):
    name = "source-" + work.name[-8:].lower().replace("_", "x")
    node = f"k3d-{name}-server-0"
    archive = work / "base.tar"
    binary = work / "released-cli.test"
    overlay = work / "source.tar.zst"
    go_overlay = work / "go-overlay.json"
    virtual = cli / "pkg/installation/source_image_test.go"
    go_overlay.write_text(json.dumps({"Replace": {str(virtual): str(HERE / "cli_test.go.txt")}}))
    run("go", "-C", cli, "test", "-c", "-overlay", go_overlay, "-o", binary, "./pkg/installation")
    run("docker", "buildx", "build", "--platform", "linux/"+ARCH, "--provenance=true", "-t", "proof:base",
        "--output", f"type=oci,dest={archive}", "-f", ROOT/"development/Dockerfile", ROOT)
    with tarfile.open(archive) as tar:
        descriptor, = json.load(tar.extractfile("index.json"))["manifests"]
        assert descriptor["mediaType"] == "application/vnd.oci.image.index.v1+json", descriptor
        base_digest = descriptor["digest"]
        def blob(digest):
            return json.load(tar.extractfile("blobs/" + digest.replace(":", "/")))
        native, = [m for m in blob(base_digest)["manifests"] if m.get("platform", {}).get("architecture") == ARCH]
        print(f"{ARCH} base compressed layer bytes {sum(layer['size'] for layer in blob(native['digest'])['layers'])}", flush=True)

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
        run(binary, "-test.run=^TestSourceImageContract$", env={**os.environ, "REGISTRY": host,
            "OVERLAY": str(overlay), **{k: str(v) for k, v in variables.items()}})

    released(BASE_OCI=archive, BASE_DIGEST=base_digest)

    def cache_image(ref):
        # The node imports the image. The app's network policy applies from its first instruction.
        ref = f"{registry}:5000/{ref}"
        run("docker", "exec", node, "ctr", "-n", "k8s.io", "images", "pull", "--plain-http", "--platform", "linux/"+ARCH, ref)
        return ref

    base = cache_image("base@"+base_digest)
    def build_pod(pod, app, image=base):
        record = json.dumps({"registry": f"http://{registry}:5000", "repository": app, "tag": "session", "token": "local"})
        run("kubectl", "create", "configmap", pod, "--from-literal=overlay.json="+record)
        proxy_url = f"http://{proxy}:3128"
        start_pod(pod, image, {"HOLON_BASE_IMAGE": image, "HOLON_OVERLAY_FILE": "/record/overlay.json",
                  "HTTP_PROXY": proxy_url, "HTTPS_PROXY": proxy_url, "http_proxy": proxy_url,
                  "https_proxy": proxy_url, "NO_PROXY": registry, "no_proxy": registry})
        status = run("kubectl", "exec", pod, "--", "timeout", "10", "bash", "-c",
                     'exec 3<>/dev/tcp/$1/3128; printf "CONNECT nodejs.org:443 HTTP/1.1\\r\\nHost: nodejs.org:443\\r\\n\\r\\n" >&3; head -1 <&3', "check-proxy", proxy)
        assert "200" in status, status

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
        print(f"{ARCH} {phase}: {seconds:.3f}s", flush=True)
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
            (project/entry).write_text(initial.replace('"first"', json.dumps(marker)))
            reply, elapsed = submit(url, project, entry, phase+" "+marker)
            assert not reply.get("output"), reply
            check_response(url, resolved, marker)
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
            run("docker", "exec", node, "ctr", "-n", "k8s.io", "images", "pull", "--platform", "linux/"+ARCH, PUBLISHED_BASE)
            control = work/"published"
            shutil.copytree(project, control)
            build_pod("python-published", app, PUBLISHED_BASE)
            with port_forward("python-published") as url:
                submit(url, control, entry, "published Python cold")
                version = check_response(url, first+".", "first")
                source_edits(url, control, entry, "published Python source-only", version)
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

    access = run("docker", "exec", name+"-proxy", "cat", "/var/log/squid/access.log")
    assert "TCP_DENIED" not in access and "nodejs.org:443" in access and "releases.astral.sh:443" in access, access
    slow = {app: samples for app, samples in timings.items() if max(samples) >= 1}
    assert not slow, f"source-only edits must finish in under a second: {slow}"


if __name__ == "__main__":
    with tempfile.TemporaryDirectory(prefix="source-image-") as tmp, contextlib.ExitStack() as resources:
        proof(Path(sys.argv[1]).resolve(), Path(tmp), resources)
