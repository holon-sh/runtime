import json
import os
import subprocess
import time
import tempfile
from urllib.request import Request, urlopen

from sdk import port_forward

def run(*args, data=None):
    return subprocess.check_output(args, input=data, text=True).strip()

from pathlib import Path

images = json.loads(Path(__file__).with_name("workers.json").read_text())
arch = run("go", "env", "GOARCH")
secrets = os.path.expanduser("~/.docker/config.json")
fixtures = Path(__file__).parent.resolve()
cli = Path(".cli").resolve()
scratch = tempfile.TemporaryDirectory()
work = Path(scratch.name)
try:
    overlay_map = work/"go-overlay.json"
    virtual = cli/"pkg/installation/source_image_test.go"
    overlay_map.write_text(json.dumps({"Replace": {str(virtual): str(fixtures/"cli_test.go.txt")}}))
    binary = work/"released-cli.test"
    run("go", "-C", str(cli), "test", "-c", "-overlay", str(overlay_map), "-o", str(binary), "./pkg/installation")
    import shutil
    project = work/"project"
    shutil.copytree(fixtures/"flows/node", project)
    packed = work/"project.tar.zst"
    subprocess.run([binary, "-test.run=^TestSourceImageContract$"], check=True,
                   env={**os.environ, "PROJECT":str(project), "ENTRY":"server.js", "OVERLAY":str(packed), "BUILD":"0"})
    source = packed.read_bytes()
finally:
    scratch.cleanup()

newest = next(row["version"].lstrip("v") for row in json.load(urlopen("https://nodejs.org/dist/index.json",timeout=30)) if row["lts"])
for index, name in enumerate(("stock", "candidate", "candidate", "stock", "stock", "candidate",
                              "candidate", "stock", "stock", "candidate")):
    image = images[name]
    cluster = f"cold-{index}"
    run("kind", "create", "cluster", "--name", cluster, "--wait", "150s")
    try:
        layers = json.loads(run("oras", "manifest", "fetch", "--platform", "linux/" + arch, image))["layers"]
        existing = set(run("docker", "exec", cluster + "-control-plane", "ctr", "-n", "k8s.io", "content", "ls", "-q").splitlines())
        cached = sum(layer["size"] for layer in layers if layer["digest"] in existing)
        if cached:
            raise RuntimeError(f"{name} already has {cached} cached bytes")
        run("kubectl", "create", "secret", "generic", "registry", "--type=kubernetes.io/dockerconfigjson",
            "--from-file=.dockerconfigjson=" + secrets)
        pod = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "worker"}, "spec": {
            "enableServiceLinks": False, "automountServiceAccountToken": False,
            "imagePullSecrets": [{"name": "registry"}], "restartPolicy": "Never",
            "securityContext": {"runAsUser": 10001, "runAsGroup": 10001, "fsGroup": 10001,
                                "seccompProfile": {"type": "RuntimeDefault"}},
            "containers": [{"name": "worker", "image": image,
                "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
                "readinessProbe": {"tcpSocket": {"port": 3000}, "periodSeconds": 1},
                "volumeMounts": [{"mountPath": "/" + volume, "name": volume} for volume in ("app", "tmp")]}],
            "volumes": [{"name": "app", "emptyDir": {"sizeLimit": "2Gi"}}, {"name": "tmp", "emptyDir": {"sizeLimit": "64Mi"}}]}}
        start = time.monotonic()
        run("kubectl", "create", "-f", "-", data=json.dumps(pod))
        try:
            run("kubectl", "wait", "pod/worker", "--for=condition=Ready", "--timeout=150s")
        except subprocess.CalledProcessError:
            print(run("kubectl", "describe", "pod", "worker"), flush=True)
            raise
        ready = time.monotonic() - start
        with port_forward("worker") as address:
            request = Request(address + "/.holon/reload", data=source, headers={"Content-Type": "application/vnd.holon.source-overlay.v0.tar+zstd"})
            start_install = time.monotonic()
            with urlopen(request, timeout=150) as response:
                reply = json.load(response)
                assert not reply.get("error"), reply
            with urlopen(address + "/runtime.json", timeout=30) as response:
                result = json.load(response)
            useful = time.monotonic()
            assert result["uid"] == 10001 and result["marker"] == "first"
        events = json.loads(run("kubectl", "get", "events", "--field-selector=involvedObject.name=worker", "-o", "json"))["items"]
        message = next(event["message"] for event in events if event["reason"] == "Pulled")
        versions = run("kubectl", "exec", "worker", "--", "sh", "-ec", "id -u; node -v; python3.13 -V; python3.15 -V").splitlines()
        assert result["version"] == (versions[1].lstrip("v") if name == "stock" else newest), result
        print("COLD", json.dumps({"arch": arch, "order": index, "image": name, "uncachedBytes": sum(l["size"] for l in layers),
                                 "cachedBytes": cached, "readySeconds": round(ready, 3), "event": message, "versions": versions,
                                 "usefulSeconds": round(useful-start,3), "installAndResponseSeconds":round(useful-start_install,3),
                                 "app":result}), flush=True)
    finally:
        run("kind", "delete", "cluster", "--name", cluster)
