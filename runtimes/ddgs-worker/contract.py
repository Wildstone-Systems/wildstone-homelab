"""Frozen W047 worker envelopes; no authority or provider configuration here."""

import json
import re
from datetime import datetime, timezone
from urllib.parse import urlsplit, urlunsplit

REQUEST_BYTES = 8192
RESPONSE_BYTES = 65536  # Internal transport; Keystone still pages into 4,000 bytes.
QUERY_BYTES = 1024
REGIONS = frozenset({"wt-wt", "us-en", "uk-en"})
ERRORS = {
    "invalid_input": (400, "Invalid search request."),
    "unsupported": (422, "Backend or filter is not supported."),
    "provider_unavailable": (503, "Search provider is unavailable."),
    "rate_limited": (429, "Search capacity is unavailable; retry later."),
    "timeout": (504, "Search deadline exceeded."),
    "cancelled": (499, "Search cancelled."),
    "internal_error": (500, "Search worker failed."),
}


class Failure(Exception):
    def __init__(self, code):
        self.code = code
        super().__init__(code)

    def response(self):
        status, message = ERRORS[self.code]
        return status, {"version": 1, "error": {"code": self.code, "message": message}}


def encode(value):
    return json.dumps(
        value, ensure_ascii=False, separators=(",", ":"), allow_nan=False
    ).encode("utf-8")


def _pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise Failure("invalid_input")
        result[key] = value
    return result


def decode(raw):
    try:
        return json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=_pairs,
            parse_constant=lambda _: (_ for _ in ()).throw(Failure("invalid_input")),
        )
    except (ValueError, UnicodeError, RecursionError):
        raise Failure("invalid_input") from None


def valid_string(value, size, nonempty=True):
    if not isinstance(value, str) or (nonempty and not value.strip()):
        return False
    try:
        return len(value.encode("utf-8")) <= size and not any(
            ord(c) < 32 or ord(c) == 127 for c in value
        )
    except UnicodeError:
        return False


def validate(value):
    required = {"version", "query", "limit", "backend"}
    if (
        not isinstance(value, dict)
        or not required <= value.keys()
        or value.keys() - required - {"filters"}
    ):
        raise Failure("invalid_input")
    if type(value["version"]) is not int or value["version"] != 1:
        raise Failure("invalid_input")
    if not valid_string(value["query"], QUERY_BYTES):
        raise Failure("invalid_input")
    if type(value["limit"]) is not int or not 1 <= value["limit"] <= 5:
        raise Failure("invalid_input")
    if not valid_string(value["backend"], 64):
        raise Failure("invalid_input")
    if value["backend"] != "duckduckgo":
        raise Failure("unsupported")
    filters = value.get("filters", {})
    if not isinstance(filters, dict) or filters.keys() - {
        "region",
        "language",
        "recencyDays",
    }:
        raise Failure("invalid_input")
    for key in ("region", "language"):
        if key in filters and not valid_string(filters[key], 1024):
            raise Failure("invalid_input")
    if "recencyDays" in filters and (
        type(filters["recencyDays"]) is not int
        or not 1 <= filters["recencyDays"] <= 36500
    ):
        raise Failure("invalid_input")
    # Do not round arbitrary day counts to DDGS d/w/m/y or silently ignore language.
    if (
        "language" in filters
        or "recencyDays" in filters
        or filters.get("region", "wt-wt") not in REGIONS
    ):
        raise Failure("unsupported")
    return value


def _text(value, maximum):
    if not isinstance(value, str):
        raise Failure("provider_unavailable")
    value = " ".join(value.split())
    if not valid_string(value, maximum, nonempty=False):
        raise Failure("provider_unavailable")
    return value


def normalize(rows, request, now=None):
    if not isinstance(rows, list):
        raise Failure("provider_unavailable")
    items = []
    seen = set()
    for row in rows[: request["limit"]]:
        if not isinstance(row, dict):
            raise Failure("provider_unavailable")
        url = row.get("href")
        if not valid_string(url, 2048) or re.search(r"\s|\\", url):
            raise Failure("provider_unavailable")
        try:
            parsed = urlsplit(url)
            if (
                parsed.scheme not in {"http", "https"}
                or not parsed.hostname
                or parsed.username is not None
                or parsed.password is not None
            ):
                raise ValueError()
            _ = parsed.port
            # No tracking rewrites, resolution, or follow-up fetches of result URLs.
            url = urlunsplit(
                (
                    parsed.scheme.lower(),
                    parsed.netloc.lower(),
                    parsed.path or "/",
                    parsed.query,
                    "",
                )
            )
        except ValueError:
            raise Failure("provider_unavailable") from None
        if url in seen:
            continue
        seen.add(url)
        item = {"url": url, "snippet": _text(row.get("body", ""), 4096)}
        title = _text(row.get("title", ""), 512)
        if title:
            item["title"] = title
        if row.get("date"):
            try:
                date = datetime.fromisoformat(row["date"].replace("Z", "+00:00"))
                if date.tzinfo is None:
                    raise ValueError()
                item["sourceDate"] = (
                    date.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
                )
            except (ValueError, TypeError, AttributeError):
                raise Failure("provider_unavailable") from None
        items.append(item)
    stamp = now or datetime.now(timezone.utc)
    result = {
        "version": 1,
        "data": {
            "items": items,
            "provider": "ddgs",
            "backend": "duckduckgo",
            "retrievedAt": stamp.astimezone(timezone.utc)
            .isoformat()
            .replace("+00:00", "Z"),
        },
    }
    if len(encode(result)) > RESPONSE_BYTES:
        raise Failure("provider_unavailable")
    return result
