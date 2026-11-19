"""Run in an exported Python role, whose standard library is already compiled."""
import sys


def cached(event, args):
    if event == "compile" and "/asyncio/" in str(args[1]):
        raise RuntimeError("the packaged asyncio bytecode did not match its source")


sys.addaudithook(cached)
import asyncio.base_events
