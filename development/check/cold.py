import json
import os
import subprocess
import time

def run(*args, data=None):
    return subprocess.check_output(args, input=data, text=True).strip()

from pathlib import Path

images = json.loads(Path(__file__).with_name("workers.json").read_text())
arch = run("go", "env", "GOARCH")
secrets = os.path.expanduser("~/.docker/config.json")
for index, name in enumerate(("stock", "candidate", "candidate", "stock", "stock", "candidate")):
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
        events = json.loads(run("kubectl", "get", "events", "--field-selector=involvedObject.name=worker", "-o", "json"))["items"]
        message = next(event["message"] for event in events if event["reason"] == "Pulled")
        versions = run("kubectl", "exec", "worker", "--", "sh", "-ec", "id -u; node -v; python3.13 -V; python3.15 -V").splitlines()
        print("COLD", json.dumps({"arch": arch, "order": index, "image": name, "uncachedBytes": sum(l["size"] for l in layers),
                                 "cachedBytes": cached, "readySeconds": round(ready, 3), "event": message, "versions": versions}), flush=True)
    finally:
        run("kind", "delete", "cluster", "--name", cluster)
