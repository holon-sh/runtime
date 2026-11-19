"""Check the cached standard library in the actual production OCI layer."""
import struct
import subprocess
import tarfile
from pathlib import Path


def check_bytecode(layout, layer):
    algorithm, digest = layer["digest"].split(":")
    blob = Path(layout, "blobs", algorithm, digest)
    # Python before 3.14 needs an external zstd decoder.
    with subprocess.Popen(["zstd", "-qdc", str(blob)], stdout=subprocess.PIPE) as zstd:
        sources, caches = {}, {}
        with tarfile.open(fileobj=zstd.stdout, mode="r|") as tar:
            for member in tar:
                path = Path(member.name)
                if path.parent.name == "asyncio" and path.suffix == ".py":
                    sources[path.stem] = int(member.mtime), member.size
                if path.parent.parts[-2:] == ("asyncio", "__pycache__") and path.suffix == ".pyc":
                    flags, mtime, size = struct.unpack("<III", tar.extractfile(member).read(16)[4:])
                    if flags == 0:
                        caches[path.name.split(".")[0]] = mtime, size
        zstd.stdout.read()
        if zstd.wait() or "base_events" not in caches or any(sources[name] != header for name, header in caches.items()):
            raise ValueError(f"{digest}: precompiled asyncio bytecode does not match its Python source")
