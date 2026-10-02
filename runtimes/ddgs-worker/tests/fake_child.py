"""No network: exercises the production process supervisor over real pipes."""

import json
import signal
import sys
import time

mode = sys.argv[1]
request = json.load(sys.stdin)
if mode == "hang":
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    time.sleep(60)
if mode == "slow":
    time.sleep(0.3)
if mode == "flood":
    sys.stdout.write("x" * 100000)
    sys.stdout.flush()
    time.sleep(60)
if mode == "malformed":
    sys.stdout.write("invalid json")
    sys.exit(0)
if mode == "crash":
    sys.stderr.write(request["query"])
    sys.exit(2)
if mode == "error":
    print(
        json.dumps(
            {
                "version": 1,
                "error": {"code": "rate_limited", "message": request["query"]},
            }
        )
    )
else:
    print(
        json.dumps(
            {
                "version": 1,
                "data": {
                    "items": [],
                    "provider": "ddgs",
                    "backend": "duckduckgo",
                    "retrievedAt": "2026-09-30T00:00:00Z",
                },
            }
        )
    )
