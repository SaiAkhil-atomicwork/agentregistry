"""Minimal A2A-spec server wrapping a Claude Haiku call.

Exposes two endpoints:
- GET /.well-known/agent.json   → agent card (A2A discovery)
- POST /                        → JSON-RPC 2.0 (`message/send` method)

Run:
  TASK=summarizer PORT=9001 ./venv/bin/python server.py
  TASK=sentiment  PORT=9002 ./venv/bin/python server.py
"""

import os
import sys
import json
import uuid
from typing import Any

import anthropic
import uvicorn
from fastapi import FastAPI, Request, Response

TASK = os.environ.get("TASK", "summarizer")
PORT = int(os.environ.get("PORT", "9001"))
HOSTNAME = os.environ.get("HOSTNAME", f"localhost:{PORT}")
# Base URL presented to clients in the agent card. When reached via
# agentgateway, the gateway strips the `/agents/<name>` prefix, so the agent
# should advertise whatever public URL it expects clients to hit. We default
# to the bare host so the JSON is predictable.
PUBLIC_URL = os.environ.get("PUBLIC_URL", f"http://{HOSTNAME}")

# Prompt templates per task. Keep them tiny and deterministic.
PROMPTS = {
    "summarizer": (
        "You are a concise summarizer. Given the user's text, reply with a 2-3 sentence "
        "summary in plain prose. No bullet points, no preamble."
    ),
    "sentiment": (
        "You are a sentiment analyzer. Return exactly one JSON object with keys "
        '`sentiment` (one of "positive"|"neutral"|"negative") and `confidence` '
        "(0-1 float). No prose, just the JSON."
    ),
}
DESCRIPTIONS = {
    "summarizer": "Summarizes a passage of text in 2-3 sentences.",
    "sentiment":  "Classifies the sentiment of a passage and returns confidence.",
}

if TASK not in PROMPTS:
    print(f"unknown TASK: {TASK} (expected one of {list(PROMPTS)})", file=sys.stderr)
    sys.exit(1)

client = anthropic.Anthropic()  # reads ANTHROPIC_API_KEY from env
app = FastAPI()


AGENT_CARD = {
    "name": TASK,
    "description": DESCRIPTIONS[TASK],
    "url": PUBLIC_URL,
    "version": "1.0.0",
    "protocolVersion": "0.2.5",
    "defaultInputModes": ["text/plain"],
    "defaultOutputModes": ["text/plain"],
    "capabilities": {"streaming": False, "pushNotifications": False},
    "skills": [
        {
            "id": f"{TASK}_run",
            "name": TASK,
            "description": DESCRIPTIONS[TASK],
            "tags": [TASK],
            "inputModes": ["text/plain"],
            "outputModes": ["text/plain"],
        }
    ],
}


@app.get("/.well-known/agent.json")
@app.get("/.well-known/agent-card.json")
def agent_card() -> dict:
    """A2A agent-card discovery endpoint."""
    return AGENT_CARD


def _extract_user_text(message: dict) -> str:
    """Collapse the A2A message `parts[]` into a single string."""
    parts = message.get("parts") or []
    chunks = []
    for p in parts:
        if isinstance(p, dict):
            if p.get("kind") == "text" and isinstance(p.get("text"), str):
                chunks.append(p["text"])
            elif isinstance(p.get("content"), str):
                chunks.append(p["content"])
        elif isinstance(p, str):
            chunks.append(p)
    return "\n".join(chunks).strip()


def _run_haiku(user_text: str) -> str:
    rsp = client.messages.create(
        model="claude-haiku-4-5-20251001",
        max_tokens=400,
        system=PROMPTS[TASK],
        messages=[{"role": "user", "content": user_text}],
    )
    return "".join(b.text for b in rsp.content if b.type == "text")


@app.post("/")
async def jsonrpc(request: Request) -> Response:
    body = await request.json()
    rpc_id = body.get("id")
    method = body.get("method", "")
    params = body.get("params") or {}

    # A2A JSON-RPC method names: message/send, message/stream, tasks/get, …
    if method not in ("message/send", "message/stream"):
        return Response(
            content=json.dumps({
                "jsonrpc": "2.0", "id": rpc_id,
                "error": {"code": -32601, "message": f"method not found: {method}"},
            }),
            media_type="application/json",
        )

    message = params.get("message") or {}
    user_text = _extract_user_text(message)
    if not user_text:
        return Response(
            content=json.dumps({
                "jsonrpc": "2.0", "id": rpc_id,
                "error": {"code": -32602, "message": "no user text in message.parts"},
            }),
            media_type="application/json",
        )

    try:
        answer = _run_haiku(user_text)
    except Exception as e:
        return Response(
            content=json.dumps({
                "jsonrpc": "2.0", "id": rpc_id,
                "error": {"code": -32000, "message": f"claude error: {e}"},
            }),
            media_type="application/json",
        )

    task_id = str(uuid.uuid4())
    ctx_id = message.get("contextId") or str(uuid.uuid4())

    # Minimal A2A Task result — `completed` status with a single assistant message.
    task = {
        "id": task_id,
        "contextId": ctx_id,
        "status": {"state": "completed"},
        "history": [
            message,
            {
                "role": "agent",
                "parts": [{"kind": "text", "text": answer}],
                "messageId": str(uuid.uuid4()),
                "contextId": ctx_id,
                "taskId": task_id,
            },
        ],
        "kind": "task",
    }
    return Response(
        content=json.dumps({"jsonrpc": "2.0", "id": rpc_id, "result": task}),
        media_type="application/json",
    )


@app.get("/healthz")
def health() -> dict:
    return {"ok": True, "task": TASK}


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=PORT)
