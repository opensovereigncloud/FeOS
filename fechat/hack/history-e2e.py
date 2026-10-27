#!/usr/bin/env python3
# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0
"""E2E: the agent really uses its tools on every turn of a conversation.

Regression test for "the agent claims tool use instead of doing it": the
backend is stateless, so the client carries the history. When a client sent
back only the rendered answer text, the model saw its own earlier turns as
results without tool calls and, from turn 3-4 on, fabricated outputs
(measured: 6/6 conversations on turn 4; 0/6 with the faithful history).

This client behaves like the frontend: it replays the backend's `history`
events verbatim. Witnesses, neither written by the code under test's reply
text: (1) `tool` SSE events, emitted only after an MCP tools/call ran,
(2) the sandbox filesystem, listed at the end.

    kubectl -n fechat port-forward deploy/fechat-backend 18080:8080 &
    fechat/hack/history-e2e.py [N=4] [URL]

All conversations share one sandbox (printed; delete it afterwards).
"""
import json, sys, time, uuid, urllib.request
from concurrent.futures import ThreadPoolExecutor

N = int(sys.argv[1]) if len(sys.argv) > 1 else 4
URL = sys.argv[2] if len(sys.argv) > 2 else "http://localhost:18080/api/v1/chat"
IMAGE = "ghcr.io/maltej/debian-ai"


def turns(f):
    return [
        f"Create the file {f} containing the output of `uname -r`, then show me its content.",
        f"Append the current date (date -u) to {f} and show me the content again.",
        f"How many bytes does {f} have now, and what is its sha256?",
        f"Make a copy of it at {f}.bak, then list /workspace with sizes.",
    ]


def chat(conv, messages):
    """One request. -> (history messages, tool events)"""
    req = urllib.request.Request(
        URL,
        data=json.dumps({"messages": messages, "conversation_id": conv, "image": IMAGE}).encode(),
        headers={"Content-Type": "application/json"},
    )
    for attempt in range(8):
        history, calls, busy = [], [], False
        with urllib.request.urlopen(req, timeout=600) as resp:
            for raw in resp:
                line = raw.decode().strip()
                if not line.startswith("data:"):
                    continue
                ev = json.loads(line[5:])
                if ev["type"] == "history":
                    history += ev["messages"]
                elif ev["type"] == "tool":
                    calls.append(ev)
                elif ev["type"] == "error":
                    if "429" not in ev["content"]:
                        raise RuntimeError(ev["content"])
                    busy = True  # gateway rate limit: same request again
        if not busy:
            return history, calls
        time.sleep(15 * (attempt + 1))
    raise RuntimeError("LLM gateway still answers 429")


def conversation(args):
    conv, f = args
    messages, per_turn = [], []
    for q in turns(f):
        messages.append({"role": "user", "content": q})
        history, calls = chat(conv, messages)
        if not history:
            raise RuntimeError("no history events — backend predates the history protocol")
        messages += history
        per_turn.append(len(calls))
    return f, per_turn


if __name__ == "__main__":
    conv = str(uuid.uuid4())
    print("conversation/sandbox:", conv, flush=True)
    chat(conv, [{"role": "user", "content": "Run `true` in the sandbox."}])  # provision once
    files = [f"/workspace/e2e-{i}.txt" for i in range(N)]
    failed = False
    with ThreadPoolExecutor(max_workers=2) as ex:
        for f, per_turn in ex.map(conversation, [(conv, f) for f in files]):
            ok = all(per_turn)
            failed |= not ok
            print("ok  " if ok else "FAIL", f, "tool calls per turn:", per_turn, flush=True)

    # witness 2: what is really on disk (positive control: the .txt files)
    _, calls = chat(conv, [{"role": "user", "content": "Run exactly `ls -1 /workspace` and nothing else."}])
    listing = "\n".join(c["output"] for c in calls).split()
    for f in files:
        for path in (f, f + ".bak"):
            present = path.rsplit("/", 1)[1] in listing
            failed |= not present
            print("ok  " if present else "FAIL", path, "on disk" if present else "MISSING on disk")
    sys.exit(1 if failed else 0)
