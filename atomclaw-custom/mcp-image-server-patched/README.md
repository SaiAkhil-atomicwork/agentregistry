# mcp-image-server-patched

Source-of-truth for the patched HuggingFace image-generation MCP that runs at
`/config/mcp-image-server/` on the prod gateway container. The upstream
`mcp-hf-images@1.2.0` targets the retired `api-inference.huggingface.co`
endpoint — this fork rewrites the HF calls to the new inference router and
adds a `generate_image_and_attach` tool that uploads directly to Paperclip's
issue-attachment endpoint so users see the PNG in the ticket UI.

## Deploy

On EC2-B (agentregistry host):

```bash
# source is checked into this repo; copy to the gateway runtime dir
sudo cp atomclaw-custom/mcp-image-server-patched/bin/cli.js \
  /tmp/arctl-runtime-stable/mcp-image-server/bin/cli.js

# if node_modules aren't already there
cp -r /tmp/mcp-image-server/node_modules /tmp/arctl-runtime-stable/mcp-image-server/

# overlay is referenced from the runtime dir directly
sudo cp atomclaw-custom/manual-overlay.prod.yaml \
  /tmp/arctl-runtime-stable/manual-overlay.yaml

# trigger a reconcile so overlay env vars bake into agent-gateway.yaml
curl -s -X POST http://localhost:12121/v0/deployments \
  -H 'content-type: application/json' \
  -d '{"serverName":"com.duckduckgo.mcp/search","version":"1.0.0","env":{},"preferRemote":false,"providerId":"local"}' \
  | jq -r .id | xargs -I{} curl -s -X DELETE http://localhost:12121/v0/deployments/{}

docker restart agentregistry_runtime-agent_gateway-1
```

## Tools

- `generate_image` — generates and saves to the gateway container's /tmp; returns the local path only. Not useful on its own from the agent side.
- `generate_image_and_attach` — generates AND uploads to Paperclip's attachment API so the image is visible in the issue UI. Required args: `prompt`, `issueId`, `companyId`. Optional: `model`, `negativePrompt`, `filename`.

Agents should read `issueId` from `$PAPERCLIP_TASK_ID` / `$ATOMCLAW_TASK_ID` and `companyId` from `$PAPERCLIP_COMPANY_ID` / `$ATOMCLAW_COMPANY_ID` (both are already injected into the adapter env by the Paperclip harness).
