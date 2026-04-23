"use client"

import { useState } from "react"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { createAgentV0 } from "@/lib/api/sdk.gen"
import type { AgentJson } from "@/lib/api/types.gen"
import { Loader2 } from "lucide-react"

interface AddAgentDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onAgentAdded: () => void
}

type DeploymentKind = "a2a-external" | "container"

// Reserved for future expansion — for now we surface the A2A-external path
// (the canonical AtomClaw use case) and defer the container path to a later
// sprint because it needs extra fields (modelProvider/modelName) that the
// registry requires. The radio group is laid out to accommodate both.
const DEFAULT_KIND: DeploymentKind = "a2a-external"

// Regex mirrors the Postgres CHECK constraint on agents.agent_name.
const AGENT_NAME_PATTERN = /^[a-zA-Z0-9][a-zA-Z0-9.-]*[a-zA-Z0-9]$/

export function AddAgentDialog({ open, onOpenChange, onAgentAdded }: AddAgentDialogProps) {
  const [kind, setKind] = useState<DeploymentKind>(DEFAULT_KIND)
  const [name, setName] = useState("")
  const [title, setTitle] = useState("")
  const [description, setDescription] = useState("")
  const [version, setVersion] = useState("1.0.0")
  const [remoteUrl, setRemoteUrl] = useState("")
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const reset = () => {
    setKind(DEFAULT_KIND)
    setName("")
    setTitle("")
    setDescription("")
    setVersion("1.0.0")
    setRemoteUrl("")
    setError(null)
    setSubmitting(false)
  }

  const closeDialog = (openState: boolean) => {
    if (!openState) reset()
    onOpenChange(openState)
  }

  const validate = (): string | null => {
    if (!name.trim()) return "Agent name is required."
    if (!AGENT_NAME_PATTERN.test(name.trim())) {
      return "Agent name may only contain letters, digits, dots, and hyphens (e.g. com.example.agent)."
    }
    if (!version.trim()) return "Version is required."
    if (!description.trim()) return "Description is required."
    if (kind === "a2a-external") {
      if (!remoteUrl.trim()) return "A2A URL is required for external agents."
      try {
        const u = new URL(remoteUrl.trim())
        if (!["http:", "https:"].includes(u.protocol)) {
          return "A2A URL must use http:// or https://"
        }
      } catch {
        return "A2A URL is not a valid URL."
      }
    }
    return null
  }

  const submit = async () => {
    setError(null)
    const msg = validate()
    if (msg) { setError(msg); return }

    const body: AgentJson = {
      name: name.trim(),
      version: version.trim(),
      title: title.trim() || undefined,
      description: description.trim(),
      // Required by the wire schema but unused for URL-only A2A agents —
      // emit empty strings so the POST validates server-side. The reconciler
      // uses Remote.URL instead when Image is empty.
      image: "",
      language: "",
      framework: "",
      modelProvider: "",
      modelName: "",
      status: "active",
      remotes: kind === "a2a-external"
        ? [{ type: "a2a", url: remoteUrl.trim() }]
        : undefined,
    }

    setSubmitting(true)
    try {
      const res = await createAgentV0({ body })
      if (res.error) {
        throw new Error(
          typeof res.error === "object" && "detail" in res.error
            ? String((res.error as { detail: unknown }).detail)
            : JSON.stringify(res.error),
        )
      }
      onAgentAdded()
      closeDialog(false)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={closeDialog}>
      <DialogContent className="max-w-2xl max-h-[80vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Add Agent</DialogTitle>
          <DialogDescription>
            Register an external A2A-spec agent or a container-based managed agent.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-4">
          <div className="space-y-2">
            <Label>Deployment type</Label>
            <div className="flex gap-4 text-sm">
              <label className="flex items-center gap-2 cursor-pointer">
                <input
                  type="radio"
                  name="kind"
                  value="a2a-external"
                  checked={kind === "a2a-external"}
                  onChange={() => setKind("a2a-external")}
                />
                External A2A (URL)
              </label>
              <label className="flex items-center gap-2 cursor-not-allowed text-muted-foreground">
                <input
                  type="radio"
                  name="kind"
                  value="container"
                  disabled
                />
                Container (coming soon)
              </label>
            </div>
          </div>

          <div className="space-y-2">
            <Label htmlFor="agent-name">Agent name *</Label>
            <Input
              id="agent-name"
              placeholder="com.example.my-agent"
              value={name}
              onChange={(e) => setName(e.target.value)}
              disabled={submitting}
            />
            <p className="text-xs text-muted-foreground">
              Letters, digits, dots, hyphens. Used as the registry key.
            </p>
          </div>

          <div className="space-y-2">
            <Label htmlFor="agent-title">Title</Label>
            <Input
              id="agent-title"
              placeholder="My Agent"
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              disabled={submitting}
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="agent-description">Description *</Label>
            <Textarea
              id="agent-description"
              placeholder="What does this agent do?"
              rows={3}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              disabled={submitting}
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="agent-version">Version *</Label>
            <Input
              id="agent-version"
              placeholder="1.0.0"
              value={version}
              onChange={(e) => setVersion(e.target.value)}
              disabled={submitting}
            />
          </div>

          {kind === "a2a-external" && (
            <div className="space-y-2">
              <Label htmlFor="agent-remote-url">A2A URL *</Label>
              <Input
                id="agent-remote-url"
                placeholder="http://host.docker.internal:9001"
                value={remoteUrl}
                onChange={(e) => setRemoteUrl(e.target.value)}
                disabled={submitting}
              />
              <p className="text-xs text-muted-foreground">
                The hostname the agentgateway forwards <code>/agents/&lt;name&gt;/*</code> requests to.
                For processes on the host machine, use <code>host.docker.internal:&lt;port&gt;</code>.
              </p>
            </div>
          )}

          {error && (
            <p className="text-sm text-red-500 border border-red-500/30 bg-red-500/5 rounded-md p-2">
              {error}
            </p>
          )}

          <div className="flex justify-end gap-2 pt-2">
            <Button variant="outline" onClick={() => closeDialog(false)} disabled={submitting}>
              Cancel
            </Button>
            <Button onClick={submit} disabled={submitting}>
              {submitting && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
              Add Agent
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}
