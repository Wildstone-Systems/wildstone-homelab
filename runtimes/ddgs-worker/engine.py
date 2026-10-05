"""Single DuckDuckGo attempt using the pinned DDGS payload/extraction engine.

Avoid DDGS's metasearch dispatcher: it falls back to auto for unknown engines,
wraps typed errors, and cannot distinguish blocked responses from empty results.
The transport is streaming, has no ambient proxy/cookie state, and never redirects.
"""

import logging
import sys
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

from contract import REQUEST_BYTES, Failure, decode, encode, normalize, validate
from ddgs.engines.duckduckgo import Duckduckgo

UPSTREAM_BYTES = 1024 * 1024
UPSTREAM_TIMEOUT = 25


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def fetch(payload):
    req = Request(
        "https://html.duckduckgo.com/html/",
        data=urlencode(payload).encode(),
        headers={
            "User-Agent": "Wildstone-DDGS/1",
            "Accept": "text/html",
            "Accept-Encoding": "identity",
        },
    )
    opener = build_opener(ProxyHandler({}), NoRedirect())
    try:
        with opener.open(req, timeout=UPSTREAM_TIMEOUT) as response:
            if response.status != 200:
                raise Failure(
                    "rate_limited"
                    if response.status in {202, 403, 429}
                    else "provider_unavailable"
                )
            if (
                response.headers.get_content_type() != "text/html"
                or response.headers.get("Content-Encoding", "identity") != "identity"
            ):
                raise Failure("provider_unavailable")
            raw = response.read(UPSTREAM_BYTES + 1)
            if len(raw) > UPSTREAM_BYTES:
                raise Failure("provider_unavailable")
            return raw.decode("utf-8")
    except HTTPError as exc:
        exc.close()
        raise Failure(
            "rate_limited" if exc.code in {403, 429} else "provider_unavailable"
        ) from None
    except TimeoutError:
        raise Failure("timeout") from None
    except URLError as exc:
        raise Failure(
            "timeout"
            if isinstance(exc.reason, TimeoutError)
            else "provider_unavailable"
        ) from None
    except (OSError, UnicodeError):
        raise Failure("provider_unavailable") from None


class Engine(Duckduckgo):
    def __init__(self, transport=fetch):
        # Use the pinned DDGS parser/payload without constructing its unbounded HTTP client.
        self.transport = transport

    def request(self, method, url, **kwargs):
        if (
            method != "POST"
            or url != "https://html.duckduckgo.com/html/"
            or set(kwargs) != {"data"}
        ):
            raise Failure("internal_error")
        text = self.transport(kwargs["data"])
        if not isinstance(text, str) or len(text.encode("utf-8")) > UPSTREAM_BYTES:
            raise Failure("provider_unavailable")
        return text

    def extract_results(self, html_text):
        tree = self.extract_tree(html_text)
        if tree.xpath(
            "//form[contains(@action, 'anomaly')] | //*[@id='challenge-form']"
        ):
            raise Failure("rate_limited")
        empty = tree.xpath(
            "//*[contains(concat(' ', normalize-space(@class), ' '), ' no-results ') "
            "or contains(concat(' ', normalize-space(@class), ' '), ' no-results__container ')]"
        )
        results = super().extract_results(html_text)
        if empty:
            # DDGS's broad body selector can turn the no-results container into
            # an empty TextResult. Only the explicit marker proves zero hits.
            if any(result.href for result in results):
                raise Failure("provider_unavailable")
            return []
        if not results:
            # A challenge, changed markup, or blank page must not look like zero hits.
            raise Failure("provider_unavailable")
        return results


def search(request, transport=fetch):
    validate(request)
    try:
        rows = Engine(transport).search(
            request["query"], region=request.get("filters", {}).get("region", "wt-wt")
        )
        if rows is None:
            raise Failure("provider_unavailable")
        return normalize([vars(row) for row in rows], request)
    except Failure:
        raise
    except Exception:  # noqa: BLE001 - redact every failure at the process/HTTP boundary
        raise Failure("provider_unavailable") from None


def main():
    logging.disable(logging.CRITICAL)
    try:
        raw = sys.stdin.buffer.read(REQUEST_BYTES + 1)
        if len(raw) > REQUEST_BYTES:
            raise Failure("invalid_input")
        result = search(validate(decode(raw)))
    except Failure as exc:
        _, result = exc.response()
    except Exception:  # noqa: BLE001 - redact every failure at the process/HTTP boundary
        _, result = Failure("internal_error").response()
    sys.stdout.buffer.write(encode(result))


if __name__ == "__main__":
    main()
