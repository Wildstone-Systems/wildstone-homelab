import asyncio
import subprocess
import sys
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest.mock import Mock, patch
from urllib.error import HTTPError, URLError

import contract
import engine
import service
from aiohttp.test_utils import TestClient, TestServer
from contract import Failure, decode, encode, normalize, validate

HERE = Path(__file__).parent
REQUEST = {
    "version": 1,
    "query": "example documentation",
    "limit": 5,
    "backend": "duckduckgo",
}


def fixture(name):
    return (HERE / "fixtures" / name).read_text()


def worker(mode="slow"):
    return service.Worker((sys.executable, "-B", str(HERE / "fake_child.py"), mode))


async def wait_for(predicate):
    async with asyncio.timeout(3):
        while not predicate():
            await asyncio.sleep(0.005)


class ContractTests(unittest.TestCase):
    def test_request_fixture(self):
        self.assertEqual(validate(decode(encode(REQUEST))), REQUEST)

    def test_strict_inputs(self):
        changes = [
            {"version": True},
            {"version": 2},
            {"limit": True},
            {"limit": 0},
            {"limit": 6},
            {"limit": 1.2},
            {"limit": "5"},
            {"query": None},
            {"query": " "},
            {"query": "é" * 513},
            {"query": "a\x00b"},
            {"query": "\ud800"},
            {"backend": None},
            {"unknown": 1},
            {"filters": None},
            {"filters": []},
            {"filters": {"unknown": "x"}},
            {"filters": {"recencyDays": True}},
            {"filters": {"recencyDays": 0}},
            {"filters": {"recencyDays": 1.5}},
            {"filters": {"region": 4}},
            {"filters": {"language": "a" * 1025}},
        ]
        for change in changes:
            with (
                self.subTest(change=repr(change)[:90]),
                self.assertRaises(Failure) as raised,
            ):
                validate(REQUEST | change)
            self.assertEqual(raised.exception.code, "invalid_input")
        for value in [
            None,
            [],
            {},
            {k: v for k, v in REQUEST.items() if k != "backend"},
        ]:
            with self.assertRaises(Failure):
                validate(value)

    def test_unsupported_is_explicit(self):
        for change in [
            {"backend": b}
            for b in ("auto", "all", "bing", "duckduckgo,bing", "DuckDuckGo")
        ] + [
            {"filters": {"language": "en"}},
            {"filters": {"recencyDays": 7}},
            {"filters": {"region": "unknown"}},
        ]:
            with self.subTest(change=change), self.assertRaises(Failure) as raised:
                validate(REQUEST | change)
            self.assertEqual(raised.exception.code, "unsupported")

    def test_invalid_json(self):
        for raw in [b'{"a":1,"a":2}', b"NaN", b"{", b"\xff", b"[" * 2000]:
            with self.assertRaises(Failure):
                decode(raw)

    def test_normalization_and_dates(self):
        rows = [
            {
                "href": "https://EXAMPLE.COM/guide#section",
                "title": " A  title ",
                "body": " A\n snippet ",
                "date": "2026-09-30T03:00:00+03:00",
            }
        ]
        now = datetime(2026, 9, 30, tzinfo=timezone.utc)
        value = normalize(rows + rows, REQUEST, now)
        self.assertEqual(
            value,
            {
                "version": 1,
                "data": {
                    "provider": "ddgs",
                    "backend": "duckduckgo",
                    "retrievedAt": "2026-09-30T00:00:00Z",
                    "items": [
                        {
                            "url": "https://example.com/guide",
                            "title": "A title",
                            "snippet": "A snippet",
                            "sourceDate": "2026-09-30T00:00:00Z",
                        }
                    ],
                },
            },
        )

    def test_invalid_upstream_fields_refuse(self):
        good = {"href": "https://example.com/", "body": "text"}
        changes = [
            {"href": u}
            for u in [
                "javascript:alert(1)",
                "file:///etc/passwd",
                "https://user@example.com/",
                "https://example.com:bad/",
                "https://"
                + "a" * 2040,  # Adding the canonical slash exceeds 2,048 bytes.
                "https://example.com/" + "a" * 2048,
                "https://exam ple.com/",
                "https://example.com\\@other.example/",
                "https://[broken/",
            ]
        ] + [
            {"title": "a" * 513},
            {"body": "é" * 2049},
            {"date": "yesterday"},
            {"date": "2026-09-30"},
            {"body": []},
        ]
        for change in changes:
            with (
                self.subTest(change=repr(change)[:80]),
                self.assertRaises(Failure) as raised,
            ):
                normalize([good | change], REQUEST)
            self.assertEqual(raised.exception.code, "provider_unavailable")

    def test_response_bounds_and_result_limit(self):
        rows = [
            {"href": f"https://example.com/{n}", "title": "a" * 512, "body": '"' * 4096}
            for n in range(10)
        ]
        result = normalize(rows, REQUEST)
        self.assertEqual(len(result["data"]["items"]), 5)
        self.assertLessEqual(len(encode(result)), contract.RESPONSE_BYTES)
        self.assertEqual(normalize([], REQUEST)["data"]["items"], [])


class EngineTests(unittest.TestCase):
    def test_pinned_ddgs_parser_and_backend(self):
        payloads = []

        def transport(payload):
            payloads.append(payload)
            return fixture("results.html")

        result = engine.search(REQUEST, transport)
        self.assertEqual(
            result["data"]["items"],
            [
                {
                    "url": "https://example.com/guide",
                    "title": "Example & documentation",
                    "snippet": "A useful snippet.",
                },
                {
                    "url": "https://example.org/",
                    "title": "Second result",
                    "snippet": "Other documentation.",
                },
            ],
        )
        self.assertEqual(payloads, [{"q": REQUEST["query"], "b": "", "l": "wt-wt"}])
        self.assertNotIn("sourceDate", result["data"]["items"][0])

    def test_empty_and_blocked_not_confused(self):
        self.assertEqual(
            engine.search(REQUEST, lambda _: fixture("empty.html"))["data"]["items"], []
        )
        for text, code in [
            (fixture("blocked.html"), "rate_limited"),
            ("<html>unexpected</html>", "provider_unavailable"),
            ("", "provider_unavailable"),
            ("x" * (engine.UPSTREAM_BYTES + 1), "provider_unavailable"),
        ]:
            with self.subTest(code=code), self.assertRaises(Failure) as raised:
                engine.search(REQUEST, lambda _, text=text: text)
            self.assertEqual(raised.exception.code, code)

    def test_no_fallback_or_retry(self):
        for code in ("timeout", "rate_limited", "provider_unavailable"):
            with patch.object(engine, "fetch", side_effect=Failure(code)) as transport:
                with self.assertRaises(Failure) as raised:
                    engine.search(REQUEST, transport)
                self.assertEqual(raised.exception.code, code)
                transport.assert_called_once()
        with patch.object(engine, "fetch") as transport:
            with self.assertRaises(Failure):
                engine.search(REQUEST | {"backend": "auto"}, transport)
            transport.assert_not_called()

    def test_transport_bounds_status_and_redirect_policy(self):
        from email.message import Message

        headers = Message()
        headers["Content-Type"] = "text/html; charset=utf-8"
        for status, body, code in [
            (200, b"x" * (engine.UPSTREAM_BYTES + 1), "provider_unavailable"),
            (202, b"challenge", "rate_limited"),
            (500, b"failed", "provider_unavailable"),
        ]:
            response = Mock(status=status, headers=headers)
            response.read.return_value = body
            with patch.object(engine, "build_opener") as opener:
                opener.return_value.open.return_value.__enter__.return_value = response
                with self.assertRaises(Failure) as raised:
                    engine.fetch({"q": "example"})
                self.assertEqual(raised.exception.code, code)
                req = opener.return_value.open.call_args.args[0]
                self.assertEqual(req.full_url, "https://html.duckduckgo.com/html/")
                if status == 200:
                    response.read.assert_called_once_with(engine.UPSTREAM_BYTES + 1)
                self.assertEqual(opener.call_args.args[0].proxies, {})
        self.assertIsNone(
            engine.NoRedirect().redirect_request(
                None, None, 302, None, None, "https://example.com/"
            )
        )

    def test_transport_errors_redacted(self):
        for exc, code in [
            (
                HTTPError("https://example.com/private", 429, "sensitive", {}, None),
                "rate_limited",
            ),
            (
                HTTPError("https://example.com/private", 403, "sensitive", {}, None),
                "rate_limited",
            ),
            (
                HTTPError("https://example.com/private", 302, "sensitive", {}, None),
                "provider_unavailable",
            ),
            (URLError("sensitive"), "provider_unavailable"),
            (TimeoutError("sensitive"), "timeout"),
        ]:
            with patch.object(engine, "build_opener") as opener:
                opener.return_value.open.side_effect = exc
                with self.assertRaises(Failure) as raised:
                    engine.fetch({"q": "sensitive"})
                self.assertEqual(raised.exception.code, code)
                self.assertNotIn("sensitive", str(raised.exception.response()))


class WorkerTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.processes = []
        popen = subprocess.Popen

        def capture(*args, **kwargs):
            proc = popen(*args, **kwargs)
            self.processes.append(proc)
            return proc

        self.patch = patch.object(service.subprocess, "Popen", side_effect=capture)
        self.patch.start()

    async def asyncTearDown(self):
        self.patch.stop()
        self.assertTrue(
            all(p.poll() is not None for p in self.processes), "child was not reaped"
        )

    async def test_two_active_eight_queued_and_overflow(self):
        w = worker()
        tasks = [asyncio.create_task(w.run(REQUEST)) for _ in range(10)]
        await wait_for(lambda: (w.active, w.queued) == (2, 8))
        self.assertEqual((w.active, w.queued), (2, 8))
        self.assertEqual(len(self.processes), 2)
        with self.assertRaises(Failure) as raised:
            await w.run(REQUEST)
        self.assertEqual(raised.exception.code, "rate_limited")
        await asyncio.gather(*tasks)
        self.assertEqual((w.active, w.queued), (0, 0))

    async def test_queued_cancel_does_not_spawn(self):
        w = worker("hang")
        tasks = [asyncio.create_task(w.run(REQUEST)) for _ in range(3)]
        await wait_for(lambda: (w.active, w.queued) == (2, 1))
        tasks[2].cancel()
        with self.assertRaises(asyncio.CancelledError):
            await tasks[2]
        self.assertEqual((w.active, w.queued, len(self.processes)), (2, 0, 2))
        for task in tasks[:2]:
            task.cancel()
        await asyncio.gather(*tasks[:2], return_exceptions=True)
        self.assertEqual(w.active, 0)

    async def test_queue_deadline_never_starts_expired_work(self):
        w = worker("hang")
        active = [asyncio.create_task(w.run(REQUEST)) for _ in range(2)]
        await wait_for(lambda: w.active == 2)
        with self.assertRaises(TimeoutError):
            async with asyncio.timeout(0.1):
                await w.run(REQUEST)
        self.assertEqual((w.active, w.queued, len(self.processes)), (2, 0, 2))
        for task in active:
            task.cancel()
        await asyncio.gather(*active, return_exceptions=True)

    async def test_hung_timeout_reaps_and_capacity_recovers(self):
        w = worker("hang")
        with self.assertRaises(TimeoutError):
            async with asyncio.timeout(0.1):
                await w.run(REQUEST)
        self.assertEqual(w.active, 0)
        w.command = worker("slow").command
        self.assertEqual((await w.run(REQUEST))["data"]["items"], [])

    async def test_error_flood_and_crash(self):
        for mode, code in [
            ("error", "rate_limited"),
            ("flood", "provider_unavailable"),
            ("malformed", "provider_unavailable"),
            ("crash", "provider_unavailable"),
        ]:
            w = worker(mode)
            with self.subTest(mode=mode), self.assertRaises(Failure) as raised:
                await w.run(REQUEST)
            self.assertEqual(raised.exception.code, code)
            self.assertEqual(w.active, 0)

    async def test_repeated_cancel_retains_slot_until_reaped(self):
        w = worker("hang")
        original = w._reap

        async def delayed(proc):
            await asyncio.sleep(0.1)
            await original(proc)

        w._reap = delayed
        task = asyncio.create_task(w.run(REQUEST))
        await wait_for(lambda: w.active == 1)
        task.cancel()
        await wait_for(lambda: bool(w.cleanups))
        task.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await task
        self.assertEqual(w.active, 1)
        await w.close()
        self.assertEqual(w.active, 0)

    async def test_unproven_reap_retains_capacity_and_fails_health(self):
        w = worker()
        await w.slots.acquire()
        w.active = 1
        proc = Mock(pid=123, stdin=None)
        proc.poll.return_value = None
        with (
            patch.object(service.os, "killpg"),
            patch.object(service, "REAP_SECONDS", 0),
        ):
            await w._reap(proc)
        self.assertFalse(w.ready)
        self.assertEqual(w.active, 1)
        client = TestClient(TestServer(service.create_app(w)))
        await client.start_server()
        self.addAsyncCleanup(client.close)
        response = await client.get("/health")
        self.assertEqual(response.status, 503)
        self.assertEqual(await response.json(), {"version": 1, "ready": False})
        response = await client.post("/v1/search", json=REQUEST)
        self.assertEqual(response.status, 503)
        self.assertEqual(len(self.processes), 0)

    async def test_http_body_admission_is_bounded(self):
        w = worker()
        server = TestServer(service.create_app(w))
        client = TestClient(server)
        await client.start_server()
        writers = []
        try:
            for _ in range(10):
                _, writer = await asyncio.open_connection(server.host, server.port)
                writers.append(writer)
                writer.write(
                    b"POST /v1/search HTTP/1.1\r\nHost: example.internal\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{"
                )
                await writer.drain()
            await wait_for(lambda: w.admitted == 10)
            self.assertEqual(w.admitted, 10)
            response = await client.post("/v1/search", json=REQUEST)
            self.assertEqual(response.status, 429)
            self.assertEqual(len(self.processes), 0)
        finally:
            for writer in writers:
                writer.close()
                await writer.wait_closed()
            await client.close()
        self.assertEqual(w.admitted, 0)

    async def test_spawn_failure_and_unready_refuse(self):
        w = service.Worker(("/nonexistent-ddgs-fixture",))
        with self.assertRaises(Failure) as raised:
            await w.run(REQUEST)
        self.assertEqual(raised.exception.code, "internal_error")
        self.assertEqual(w.active, 0)
        w.ready = False
        with self.assertRaises(Failure) as raised:
            await w.run(REQUEST)
        self.assertEqual(raised.exception.code, "provider_unavailable")

    async def test_shutdown_cancels_active_and_queued(self):
        w = worker("hang")
        client = TestClient(TestServer(service.create_app(w)))
        await client.start_server()
        calls = [
            asyncio.create_task(client.post("/v1/search", json=REQUEST))
            for _ in range(3)
        ]
        await wait_for(lambda: (w.active, w.queued) == (2, 1))
        self.assertEqual((w.active, w.queued), (2, 1))
        await client.close()
        await asyncio.gather(*calls, return_exceptions=True)
        self.assertEqual((w.active, w.queued, w.admitted), (0, 0, 0))
        self.assertFalse(w.ready)

    async def test_slow_body_deadline_and_compression_refused(self):
        w = worker()
        server = TestServer(service.create_app(w, deadline=0.15))
        client = TestClient(server)
        await client.start_server()
        self.addAsyncCleanup(client.close)
        response = await client.post(
            "/v1/search",
            data=b"invalid compressed bytes",
            headers={"Content-Type": "application/json", "Content-Encoding": "gzip"},
        )
        self.assertEqual(response.status, 400)
        reader, writer = await asyncio.open_connection(server.host, server.port)
        writer.write(
            b"POST /v1/search HTTP/1.1\r\nHost: example.internal\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{"
        )
        await writer.drain()
        status = await asyncio.wait_for(reader.readline(), 1)
        self.assertIn(b"504", status)
        writer.close()
        await writer.wait_closed()
        self.assertEqual(len(self.processes), 0)

    async def test_http_contract_errors_health_and_safe_logs(self):
        w = worker("error")
        client = TestClient(TestServer(service.create_app(w)))
        await client.start_server()
        self.addAsyncCleanup(client.close)
        with self.assertLogs(service.LOG, level="INFO") as logs:
            health = await client.get("/health")
            self.assertEqual(await health.json(), {"version": 1, "ready": True})
            search = await client.post(
                "/v1/search", json=REQUEST | {"query": "PRIVATE-SENTINEL"}
            )
            self.assertEqual(search.status, 429)
            self.assertEqual(await search.json(), Failure("rate_limited").response()[1])
            for data in [b"{", b"x" * (contract.REQUEST_BYTES + 1), b'{"version":NaN}']:
                response = await client.post(
                    "/v1/search",
                    data=data,
                    headers={"Content-Type": "application/json"},
                )
                self.assertEqual(response.status, 400)
            response = await client.post("/v1/search?PRIVATE-SENTINEL", json=REQUEST)
            self.assertEqual(response.status, 400)
            response = await client.get("/PRIVATE-SENTINEL")
            self.assertEqual(response.status, 404)
            self.assertNotIn("PRIVATE-SENTINEL", str(logs.output))
        self.assertEqual(len(self.processes), 1)

    async def test_http_disconnect_kills_active_work(self):
        w = worker("hang")
        server = TestServer(service.create_app(w))
        await server.start_server()
        self.addAsyncCleanup(server.close)
        _reader, writer = await asyncio.open_connection(server.host, server.port)
        raw = encode(REQUEST)
        writer.write(
            b"POST /v1/search HTTP/1.1\r\nHost: example.internal\r\nContent-Type: application/json\r\nContent-Length: "
            + str(len(raw)).encode()
            + b"\r\n\r\n"
            + raw
        )
        await writer.drain()
        await wait_for(lambda: w.active == 1)
        self.assertEqual(w.active, 1)
        writer.close()
        await writer.wait_closed()
        for _ in range(100):
            if w.active == 0:
                break
            await asyncio.sleep(0.01)
        self.assertEqual(w.active, 0)
        self.assertEqual(w.admitted, 0)

    async def test_http_deadline_cleans_up_every_request(self):
        w = worker("hang")
        client = TestClient(TestServer(service.create_app(w, deadline=0.15)))
        await client.start_server()
        self.addAsyncCleanup(client.close)
        responses = await asyncio.gather(
            *(client.post("/v1/search", json=REQUEST) for _ in range(3))
        )
        self.assertEqual([r.status for r in responses], [504, 504, 504])
        self.assertEqual((w.active, w.queued, w.admitted), (0, 0, 0))


if __name__ == "__main__":
    unittest.main()
