# A2A test agents

Two minimal A2A-spec HTTP servers used to smoke-test the `kind: Agent` URL-only
path and AgentGateway's A2A policy. Each wraps a Claude Haiku call.

## Run locally

```bash
cd atomclaw-custom/a2a-test-agents
python3 -m venv venv
./venv/bin/pip install -r requirements.txt

export ANTHROPIC_API_KEY=...
TASK=summarizer PORT=9001 ./venv/bin/python server.py &
TASK=sentiment  PORT=9002 ./venv/bin/python server.py &
```

## Register + deploy via arctl

```bash
arctl apply -f summarizer.yaml
arctl apply -f sentiment.yaml
arctl deployments create com.atomclaw.a2a.summarizer --type agent --version 1.0.0
arctl deployments create com.atomclaw.a2a.sentiment  --type agent --version 1.0.0
```

The reconciler emits routes on agentgateway at
`/agents/<deployment-scoped-name>` pointing at `host.docker.internal:9001/9002`
with the `a2a: {}` policy (and URLRewrite stripping the prefix).

## Smoke probe

```bash
# agent card
curl http://localhost:21212/agents/<deployment-scoped-name>/.well-known/agent.json

# JSON-RPC round-trip
curl -X POST http://localhost:21212/agents/<deployment-scoped-name>/ \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"…"}]}}}'
```
