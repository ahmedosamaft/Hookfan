"""A minimal hookfan receiver, used by `make demo` to show fan-out working.

Mirrors the Python snippet shown in the UI: validate the token, verify the
HMAC over the raw body, echo the link-verify challenge, log real events.
"""
import hashlib
import hmac
import json
import os
from datetime import datetime, timezone

from fastapi import BackgroundTasks, FastAPI, Header, Request, Response

app = FastAPI()

NAME = os.environ.get("RECEIVER_NAME", "receiver")
LINK_TOKEN = os.environ.get("HOOKFAN_TOKEN", "").encode()
# "500" makes every forwarded event fail, so retries and the circuit breaker
# are visible in the demo.
MODE = os.environ.get("MODE", "")

received: list[dict] = []


def log(message: str) -> None:
    stamp = datetime.now(timezone.utc).strftime("%H:%M:%S")
    print(f"[{stamp}] {NAME}: {message}", flush=True)


@app.post("/hook")
async def receive(
    request: Request,
    background: BackgroundTasks,
    x_hookfan_token: str | None = Header(default=None),
    x_hookfan_signature: str | None = Header(default=None),
    x_hookfan_event: str | None = Header(default=None),
    x_hookfan_event_id: str | None = Header(default=None),
    x_hookfan_attempt: str | None = Header(default=None),
):
    body = await request.body()

    if not hmac.compare_digest((x_hookfan_token or "").encode(), LINK_TOKEN):
        log("REJECTED: wrong token")
        return Response(status_code=401)

    want = hmac.new(LINK_TOKEN, body, hashlib.sha256).hexdigest()
    got = (x_hookfan_signature or "").removeprefix("sha256=")
    if not hmac.compare_digest(want, got):
        log("REJECTED: bad signature")
        return Response(status_code=401)

    if x_hookfan_event == "link.verify":
        log("link.verify OK — echoing challenge")
        return {"challenge": json.loads(body)["challenge"]}

    log(f"EVENT id={x_hookfan_event_id} attempt={x_hookfan_attempt} bytes={len(body)}")

    if MODE == "500":
        log("  returning 500 (MODE=500) — hookfan will retry")
        return Response(status_code=500)

    background.add_task(process, body)
    return Response(status_code=200)


def process(body: bytes) -> None:
    received.append(json.loads(body))


@app.get("/received")
async def list_received():
    """Lets the demo assert what actually arrived."""
    return {"name": NAME, "count": len(received), "events": received}
