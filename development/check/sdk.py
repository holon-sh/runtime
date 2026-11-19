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
REGISTRY = "registry:3.1.1"
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

    def cache_image(ref):
        # The node imports the image. The app's network policy applies from its first instruction.
        ref = f"{registry}:5000/{ref}"
        run("docker", "exec", node, "ctr", "-n", "k8s.io", "images", "pull", "--plain-http", "--platform", "linux/"+ARCH, ref)
        return ref

    def mirror(image, label):
        run("oras", "cp", "--to-plain-http", image, host+"/"+label+":check")
        return cache_image(label+"@"+image.split("@")[1])

    refs = json.loads((HERE/"workers.json").read_text())
    base = mirror(refs["candidate"], "base")
    old_base = mirror(refs["stock"], "oldbase")
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

    def block_external(pod):
        result = run("kubectl","exec",pod,"--","python3.13","-c",
            "import socket,sys; s=socket.socket(); s.settimeout(2); print(s.connect_ex((sys.argv[1],5000)))",registry)
        assert int(result) != 0, "production retained registry egress"
    def timed(fn, *args, **kwargs):
        started = time.monotonic()
        value = fn(*args, **kwargs)
        return value, time.monotonic()-started

    run("uv", "lock", "--directory", HERE/"flows/python", "--default-index", "https://pypi.org/simple")
    digests = {}
    for app, expected, trials, stock_supports in [
        ("python","3.15.0",3,True), ("python","3.13.16",3,True), ("node","24.22.0",3,True), ("node","26.13.1",2,False), ("node","implicit",3,True),
    ]:
        entry = "server.js" if app == "node" else "server.py"
        manager_version = "10.34.9"
        pattern = app+"-"+expected
        project = work/pattern
        fixture = app
        shutil.copytree(HERE/"flows"/fixture,project)
        if expected != "implicit":
            version_file = ".python-version" if app == "python" else ".nvmrc"
            (project/version_file).write_text(expected + "\n")
        if expected == "implicit":
            assert set(json.loads((project/"package.json").read_text())).isdisjoint({"devEngines","engines"})
            assert not (project/".node-version").exists()
            upstream_lts = next(r["version"].lstrip("v") for r in request("https://nodejs.org/dist/index.json") if r["lts"])


        def observed(url, marker):
            answer = request(url+"/runtime.json")
            actual = ("24.22.0" if side == "dev20" else upstream_lts) if expected == "implicit" else expected
            assert answer["version"] == actual and answer["marker"] == marker and answer["uid"] == 10001, answer
            return answer

        for rep in range(trials):
            variants = [("dev20", old_base, "oldbase"), ("candidate", base, "base")]
            if not stock_supports: variants = variants[1:]
            if rep % 2:
                variants.reverse()
            for side, image, base_repository in variants:
                sample = f"{app}-{expected.replace(chr(46), chr(45))}-{side}-{rep}"
                dev, build, production = (sample+"-"+phase for phase in ("dev", "build", "prod"))
                measurements = {}
                shutil.copyfile(HERE/"flows"/fixture/entry, project/entry)
                measurements["dev_pod_ready"] = build_pod(dev, sample, image)
                with port_forward(dev) as url:
                    t_dev_post = time.monotonic()
                    ((_, post), total) = timed(submit, url, project, entry, sample+" cold dev")
                    measurements.update(dev_cold_cli_and_ready=total, dev_cold_post=post)
                    answer = observed(url, "first")
                    measurements["dev_cli_and_useful"] = time.monotonic()-t_dev_post
                    measurements["dev_pod_and_cli_to_useful"] = measurements["dev_pod_ready"] + measurements["dev_cli_and_useful"]
                    print("OBSERVED",pattern,side,rep+1,answer,flush=True)
                    if app == "node":
                        actual = run("kubectl", "exec", dev, "--", "sh", "-c", "cd /app/source && pnpm --version")
                        assert actual == manager_version, (actual, manager_version)
                    edits = []
                    old = (project/entry).read_text()
                    for marker in ["save-1","save-2","save-3"]:
                        began = time.monotonic()
                        (project/entry).write_text(old.replace('"first"',json.dumps(marker)))
                        reply, post = submit(url, project, entry, sample+" "+marker)
                        assert not reply.get("output"), reply
                        observed(url, marker)
                        edits.append([round(post,3),round(time.monotonic()-began,3)])
                    print("SAVES",sample,json.dumps(edits),flush=True)
                    assert all(post < 1 for post, elapsed in edits), edits
                run("kubectl", "delete", "pod", dev, "--wait=false")
                measurements["build_pod_ready"] = build_pod(build, sample, image)
                with port_forward(build) as url:
                    ((reply, post), total) = timed(submit, url, project, entry, sample+" deploy build", build=True)
                    measurements.update(deploy_cli_and_freeze=total, build_freeze_post=post)
                _, measurements["cli_assemble"] = timed(released, FROZEN=reply["image"], APP=sample, ENTRY=entry,
                                                         BASE_REPOSITORY=base_repository)
                if side == "candidate":
                    assert digests.setdefault(pattern, reply["image"]) == reply["image"], (pattern, digests, reply)
                media = {"Accept": "application/vnd.oci.image.manifest.v1+json"}
                manifests = f"http://{host}/v2/{sample}/manifests/"
                assembled = request(manifests+"production", headers=media)
                layers = request(manifests+reply["image"], headers=media)["layers"]
                sizes = {"base": sum(l["size"] for l in assembled["layers"][:-len(layers)]),
                         "frozen": [l["size"] for l in layers], "total": sum(l["size"] for l in assembled["layers"])}
                assert assembled["layers"][-len(layers):] == layers, "assembled image did not append the frozen layers"
                with urlopen(Request(manifests+"production",headers=media),timeout=10) as response:
                    print("DIGEST",pattern,side,rep+1,reply["image"],response.headers["Docker-Content-Digest"],flush=True)
                print("BYTES", ARCH, pattern, side, rep+1, json.dumps(sizes), flush=True)
                frozen, measurements["pull_assembled"] = timed(cache_image, sample+":production")
                began = time.monotonic()
                start_pod(production, frozen, {"HOST":"0.0.0.0"}, production=True)
                with port_forward(production) as url:
                    observed(url, "save-3")
                measurements["production_ready_and_observed"] = time.monotonic()-began
                block_external(production)
                print("OFFLINE",sample,"production verified",flush=True)
                measurements["deploy_total"] = sum(measurements[k] for k in
                    ["build_pod_ready","deploy_cli_and_freeze","cli_assemble","pull_assembled","production_ready_and_observed"])
                elapsed = {key: round(value, 3) for key, value in measurements.items()}
                print("BENCH", ARCH, pattern, side, rep+1, json.dumps(elapsed), flush=True)
                run("kubectl", "delete", "pod", build, production, "--wait=false")
    hybrid = work/"hybrid"
    shutil.copytree(HERE/"flows/python",hybrid)
    (hybrid/".python-version").write_text("3.12.13\n")
    (hybrid/"package.json").write_text(json.dumps({"name":"page","private":True,"engines":{"node":"26.13.1"},
        "dependencies":{"ms":"2.1.3"},"scripts":{"build":"node page.cjs"}}))
    (hybrid/"page.cjs").write_text("const fs = require('fs'); fs.mkdirSync('dist',{recursive:true}); fs.writeFileSync('dist/page.json',JSON.stringify({minute:require('ms')('1m'),node:process.version}));")
    (hybrid/"server.py").write_text("from pathlib import Path\nfrom mcp.server import MCPServer\nserver=MCPServer('page')\nfrontend=Path(__file__).with_name('dist')\n")
    build_pod("hybrid-build","hybrid")
    with port_forward("hybrid-build") as url:
        reply, _ = submit(url,hybrid,"server.py","hybrid",build=True)
    released(FROZEN=reply["image"],APP="hybrid",ENTRY="server.py")
    released_image=cache_image("hybrid:production")
    start_pod("hybrid-offline",released_image,{"HOST":"0.0.0.0"},production=True)
    with port_forward("hybrid-offline") as url: result=request(url+"/page.json")
    assert result == {"minute":60000,"node":"v26.13.1"},result
    block_external("hybrid-offline")
    checks=run("kubectl","exec","hybrid-offline","--","sh","-ec","cd /app/source; ls -d .venv .toolchain/python; test ! -e .toolchain/node; test ! -e node_modules; .venv/bin/python -V; /usr/local/bin/node -v; test \"$(.venv/bin/python -V)\" = 'Python 3.12.13'")
    print("HYBRID",result,checks,flush=True)
    access=run("docker","exec",name+"-proxy","cat","/var/log/squid/access.log")
    print("EGRESS", sorted(set(re.findall(r"CONNECT ([^ ]+)", access))),flush=True)
    assert "TCP_DENIED" not in access and "nodejs.org:443" in access and "releases.astral.sh:443" in access


if __name__ == "__main__":
    with tempfile.TemporaryDirectory(prefix="source-image-") as tmp, contextlib.ExitStack() as resources:
        proof(Path(sys.argv[1]).resolve(), Path(tmp), resources)
