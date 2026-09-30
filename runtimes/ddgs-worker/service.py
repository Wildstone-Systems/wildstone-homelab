"""One service process per installation: two killable attempts, eight waiters."""

import asyncio
import logging
import os
import signal
import subprocess
import sys
from pathlib import Path

from aiohttp import ClientPayloadError, web
from contract import (
    ERRORS,
    REQUEST_BYTES,
    RESPONSE_BYTES,
    Failure,
    decode,
    encode,
    validate,
)

LOG = logging.getLogger("wildstone.ddgs")
DEADLINE = 30
REAP_SECONDS = 1


def event(name, **counts):
    # Callers supply only fixed labels/counts. Never log requests or exceptions.
    LOG.info(encode({"event": name, **counts}).decode())


class Worker:
    def __init__(self, command=None):
        self.command = command or (
            sys.executable,
            "-B",
            str(Path(__file__).with_name("engine.py")),
        )
        self.slots = asyncio.Semaphore(2)
        self.active = 0
        self.queued = 0
        self.admitted = 0
        self.ready = True
        self.cleanups = set()

    async def _reap(self, proc):
        if proc is not None:
            # Kill the session's process group, including any unexpected descendants.
            try:
                # A reaped PID could already have been reused; never signal it.
                if proc.returncode is None:
                    os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            except OSError:
                self.ready = False
                event("reap_failed", active=self.active, queued=self.queued)
                return
            end = asyncio.get_running_loop().time() + REAP_SECONDS
            while proc.poll() is None and asyncio.get_running_loop().time() < end:
                await asyncio.sleep(0.01)
            for stream in (proc.stdin, proc.stdout):
                if stream is not None:
                    try:
                        stream.close()
                    except OSError:
                        pass  # Closed pipes do not change proof of process exit.
            if proc.poll() is None:
                # Never recycle capacity while an upstream process may still execute.
                self.ready = False
                event("reap_failed", active=self.active, queued=self.queued)
                return
        self.active -= 1
        self.slots.release()
        event("released", active=self.active, queued=self.queued)

    async def run(self, request):
        if not self.ready:
            raise Failure("provider_unavailable")
        if self.slots.locked() and self.queued >= 8:
            raise Failure("rate_limited")
        self.queued += 1
        event("queued", active=self.active, queued=self.queued)
        try:
            await self.slots.acquire()
        finally:
            self.queued -= 1
        self.active += 1
        proc = None
        try:
            if not self.ready:
                raise Failure("provider_unavailable")
            event("started", active=self.active, queued=self.queued)
            # Popen is synchronous so cancellation cannot lose a just-created child.
            # No query in argv/environment. The validated UTF-8 request fits a pipe.
            proc = subprocess.Popen(  # noqa: ASYNC220 - atomic child ownership across cancellation
                self.command,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                start_new_session=True,
                env={
                    "PATH": os.defpath,
                    "LANG": "C.UTF-8",
                    "PYTHONDONTWRITEBYTECODE": "1",
                },
            )
            proc.stdin.write(encode(request))
            proc.stdin.close()
            proc.stdin = None
            os.set_blocking(proc.stdout.fileno(), False)
            raw = bytearray()
            while True:
                try:
                    chunk = os.read(
                        proc.stdout.fileno(), min(8192, RESPONSE_BYTES + 1 - len(raw))
                    )
                except BlockingIOError:
                    await asyncio.sleep(0.01)
                    continue
                if chunk:
                    raw.extend(chunk)
                    if len(raw) > RESPONSE_BYTES:
                        raise Failure("provider_unavailable")
                elif proc.poll() is not None:
                    break
                await asyncio.sleep(0.01)
            if proc.returncode != 0:
                raise Failure("provider_unavailable")
            try:
                result = decode(raw)
            except Failure:
                raise Failure("provider_unavailable") from None
            if isinstance(result, dict) and "error" in result:
                code = result["error"].get("code")
                raise Failure(code if code in ERRORS else "provider_unavailable")
            if (
                not isinstance(result, dict)
                or set(result) != {"version", "data"}
                or result["version"] != 1
            ):
                raise Failure("provider_unavailable")
            return result
        except asyncio.CancelledError:
            event("cancelled", active=self.active, queued=self.queued)
            raise
        except Failure:
            raise
        except Exception:  # noqa: BLE001 - redact every failure at the process/HTTP boundary
            raise Failure("internal_error") from None
        finally:
            cleanup = asyncio.create_task(self._reap(proc))
            self.cleanups.add(cleanup)
            cleanup.add_done_callback(self.cleanups.discard)
            # Repeated client cancellation cannot release a slot before reaping.
            await asyncio.shield(cleanup)

    async def close(self):
        self.ready = False
        if self.cleanups:
            await asyncio.gather(*self.cleanups)


def response(status, body):
    return web.Response(
        status=status,
        body=encode(body),
        content_type="application/json",
        headers={"Cache-Control": "no-store"},
    )


def create_app(worker=None, deadline=DEADLINE):
    worker = worker or Worker()
    inflight = set()

    @web.middleware
    async def boundary(request, handler):
        admitted = False
        try:
            if request.query_string:
                raise Failure("invalid_input")
            if request.path == "/v1/search" and request.method == "POST":
                if not worker.ready:
                    raise Failure("provider_unavailable")
                if worker.admitted >= 10:
                    raise Failure("rate_limited")
                worker.admitted += 1
                event("admitted", admitted=worker.admitted)
                admitted = True
                inflight.add(asyncio.current_task())
            async with asyncio.timeout(deadline):
                return await handler(request)
        except TimeoutError:
            exc = Failure("timeout")
        except Failure as failure:
            exc = failure
        except ClientPayloadError:
            exc = Failure("invalid_input")
        except web.HTTPException as failure:
            exc = Failure("invalid_input")
            if failure.status == 404:
                return response(404, exc.response()[1])
            if failure.status == 405:
                return response(405, exc.response()[1])
        except asyncio.CancelledError:
            event("cancelled")
            raise
        except Exception:  # noqa: BLE001 - redact every failure at the process/HTTP boundary
            exc = Failure("internal_error")
        finally:
            if admitted:
                worker.admitted -= 1
                inflight.discard(asyncio.current_task())
        event(exc.code)
        status, body = exc.response()
        return response(status, body)

    async def health(request):
        return response(
            200 if worker.ready else 503, {"version": 1, "ready": worker.ready}
        )

    async def search(request):
        if (
            request.content_type != "application/json"
            or request.headers.get("Content-Encoding", "identity") != "identity"
        ):
            raise Failure("invalid_input")
        raw = await request.read()
        if len(raw) > REQUEST_BYTES:
            raise Failure("invalid_input")
        value = validate(decode(raw))
        result = await worker.run(value)
        event("completed")
        return response(200, result)

    async def shutdown(app):
        worker.ready = False
        tasks = tuple(inflight)
        for task in tasks:
            task.cancel()
        if tasks:
            await asyncio.gather(*tasks, return_exceptions=True)

    async def close(app):
        await worker.close()

    app = web.Application(
        middlewares=[boundary],
        client_max_size=REQUEST_BYTES,
        handler_args={"handler_cancellation": True, "auto_decompress": False},
    )
    app.router.add_get("/health", health, allow_head=False)
    app.router.add_post("/v1/search", search)
    app.on_shutdown.append(shutdown)
    app.on_cleanup.append(close)
    return app


def main():
    # Framework/parser/access logs may contain paths, headers, or malformed bytes.
    logging.basicConfig(level=logging.CRITICAL)
    LOG.setLevel(logging.INFO)
    from importlib.metadata import version

    if version("ddgs") != "9.16.0" or version("aiohttp") != "3.14.3":
        event("dependency_mismatch")
        raise SystemExit(1)
    host = os.environ.get("DDGS_LISTEN_HOST", "127.0.0.1")
    web.run_app(
        create_app(),
        host=host,
        port=8080,
        access_log=None,
        handler_cancellation=True,
        shutdown_timeout=2,
        print=None,
    )


if __name__ == "__main__":
    main()
