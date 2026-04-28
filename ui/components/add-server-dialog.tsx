"use client"

import { useState } from "react"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { createServerV0, type ServerJson, type KeyValueInput, type Transport } from "@/lib/admin-api"
import { Loader2, AlertCircle, Plus, Trash2 } from "lucide-react"
import { toast } from "sonner"

interface AddServerDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onServerAdded: () => void
}

export function AddServerDialog({ open, onOpenChange, onServerAdded }: AddServerDialogProps) {
  const [loading, setLoading] = useState(false)

  // Form fields
  const [schema, setSchema] = useState("2025-10-17")
  const [name, setName] = useState("")
  const [title, setTitle] = useState("")
  const [description, setDescription] = useState("")
  const [version, setVersion] = useState("")
  const [websiteUrl, setWebsiteUrl] = useState("")
  const [repositorySource, setRepositorySource] = useState<"github" | "gitlab" | "bitbucket">("github")
  const [repositoryUrl, setRepositoryUrl] = useState("")

  // Dynamic fields
  const [packages, setPackages] = useState<Array<{ identifier: string; version: string; registryType: string; transport: string }>>([])
  // Remote headers are DECLARATIONS, not values: the registry merges HEADER_<name>
  // env vars at deploy-time only for headers listed here (see processHeaders in
  // agentregistry/internal/registry/platforms/utils/deployment_adapter_utils.go).
  // Leaving a required header without a value forces the deployer to supply it.
  const [remotes, setRemotes] = useState<Array<{
    type: string;
    url: string;
    headers: Array<{
      name: string;
      value: string;
      isRequired: boolean;
      isSecret: boolean;
      description: string;
    }>;
  }>>([])

  const resetForm = () => {
    setSchema("2025-10-17")
    setName("")
    setTitle("")
    setDescription("")
    setVersion("")
    setWebsiteUrl("")
    setRepositoryUrl("")
    setPackages([])
    setRemotes([])
  }

  const handleSubmit = async () => {
    setLoading(true)

    try {
      // Validate required fields
      if (!name.trim()) {
        throw new Error("Server name is required")
      }
      
      // Validate name format (namespace/name)
      const namePattern = /^[a-zA-Z0-9.-]+\/[a-zA-Z0-9._-]+$/
      if (!namePattern.test(name.trim())) {
        throw new Error("Server name must be in format 'namespace/name' (e.g., 'io.example/my-server')")
      }
      
      if (!version.trim()) {
        throw new Error("Version is required")
      }
      if (!description.trim()) {
        throw new Error("Description is required")
      }

      // Build server object
      const server: ServerJson = {
        $schema: schema.trim(),
        name: name.trim(),
        description: description.trim(),
        version: version.trim(),
      }

      if (title.trim()) {
        server.title = title.trim()
      }

      if (websiteUrl.trim()) {
        server.websiteUrl = websiteUrl.trim()
      }

      if (repositoryUrl.trim()) {
        server.repository = {
          source: repositorySource,
          url: repositoryUrl.trim(),
        }
      }

      if (packages.length > 0) {
        server.packages = packages
          .filter(p => p.identifier.trim() && p.version.trim())
          .map(p => ({
            identifier: p.identifier.trim(),
            version: p.version.trim(),
            registryType: p.registryType as 'npm' | 'pypi' | 'docker',
            transport: { type: p.transport || 'stdio' },
          }))
      }

      if (remotes.length > 0) {
        server.remotes = remotes
          .filter(r => r.type.trim())
          .map<Transport>(r => {
            const headers: KeyValueInput[] = (r.headers || [])
              .filter(h => h.name.trim())
              .map(h => {
                const entry: KeyValueInput = { name: h.name.trim() }
                if (h.value.trim()) entry.value = h.value.trim()
                if (h.isRequired) entry.isRequired = true
                if (h.isSecret) entry.isSecret = true
                if (h.description.trim()) entry.description = h.description.trim()
                return entry
              })
            const remote: Transport = {
              type: r.type.trim(),
              url: r.url.trim() || undefined,
            }
            if (headers.length > 0) remote.headers = headers
            return remote
          })
      }

      // Create server
      const { data } = await createServerV0({ body: server, throwOnError: true })

      // Show success toast
      toast.success(`Server "${data?.server.name}" created successfully!`)

      // Close dialog and refresh
      onOpenChange(false)
      onServerAdded()
      resetForm()
    } catch (err) {
      // Show error toast
      toast.error(err instanceof Error ? err.message : "Failed to create server")
    } finally {
      setLoading(false)
    }
  }

  const addPackage = () => {
    setPackages([...packages, { identifier: "", version: "", registryType: "npm", transport: "stdio" }])
  }

  const removePackage = (index: number) => {
    setPackages(packages.filter((_, i) => i !== index))
  }

  const updatePackage = (index: number, field: string, value: string) => {
    const updated = [...packages]
    updated[index] = { ...updated[index], [field]: value }
    setPackages(updated)
  }

  const addRemote = () => {
    setRemotes([...remotes, { type: "streamable-http", url: "", headers: [] }])
  }

  const removeRemote = (index: number) => {
    setRemotes(remotes.filter((_, i) => i !== index))
  }

  const updateRemote = (index: number, field: string, value: string) => {
    const updated = [...remotes]
    updated[index] = { ...updated[index], [field]: value }
    setRemotes(updated)
  }

  const addHeader = (remoteIdx: number) => {
    const updated = [...remotes]
    updated[remoteIdx] = {
      ...updated[remoteIdx],
      headers: [
        ...(updated[remoteIdx].headers || []),
        { name: "", value: "", isRequired: true, isSecret: true, description: "" },
      ],
    }
    setRemotes(updated)
  }

  const removeHeader = (remoteIdx: number, headerIdx: number) => {
    const updated = [...remotes]
    updated[remoteIdx] = {
      ...updated[remoteIdx],
      headers: updated[remoteIdx].headers.filter((_, i) => i !== headerIdx),
    }
    setRemotes(updated)
  }

  const updateHeader = (
    remoteIdx: number,
    headerIdx: number,
    field: "name" | "value" | "description" | "isRequired" | "isSecret",
    value: string | boolean,
  ) => {
    const updated = [...remotes]
    const headers = [...(updated[remoteIdx].headers || [])]
    headers[headerIdx] = { ...headers[headerIdx], [field]: value }
    updated[remoteIdx] = { ...updated[remoteIdx], headers }
    setRemotes(updated)
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-6xl max-h-[90vh] overflow-y-auto px-8">
        <DialogHeader>
          <DialogTitle>Add New MCP Server</DialogTitle>
          <DialogDescription>
            Manually add a new MCP server to your registry
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-4">
          {/* Basic Information */}
          <div className="grid grid-cols-3 gap-4">
            <div className="space-y-2">
              <Label htmlFor="name">Server Name *</Label>
              <Input
                id="name"
                placeholder="io.example/my-server"
                value={name}
                onChange={(e) => setName(e.target.value)}
                disabled={loading}
                className={name && !/^[a-zA-Z0-9.-]+\/[a-zA-Z0-9._-]+$/.test(name) ? "border-yellow-500" : ""}
              />
              <p className={`text-xs flex items-center gap-1 min-h-[1.25rem] ${name && !/^[a-zA-Z0-9.-]+\/[a-zA-Z0-9._-]+$/.test(name) ? 'text-yellow-600' : 'invisible'}`}>
                <AlertCircle className="h-3 w-3" />
                Must be in format namespace/name (e.g., io.example/my-server)
              </p>
            </div>

            <div className="space-y-2">
              <Label htmlFor="title">Display Title</Label>
              <Input
                id="title"
                placeholder="My Server"
                value={title}
                onChange={(e) => setTitle(e.target.value)}
                disabled={loading}
              />
            </div>

            <div className="space-y-2">
              <Label htmlFor="version">Version *</Label>
              <Input
                id="version"
                placeholder="1.0.0"
                value={version}
                onChange={(e) => setVersion(e.target.value)}
                disabled={loading}
              />
            </div>
          </div>

          <div className="space-y-2">
            <Label htmlFor="description">Description *</Label>
            <Textarea
              id="description"
              placeholder="Describe what this server does..."
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              rows={3}
              disabled={loading}
            />
          </div>

          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-2">
              <Label htmlFor="websiteUrl">Website URL</Label>
              <Input
                id="websiteUrl"
                placeholder="https://example.com"
                value={websiteUrl}
                onChange={(e) => setWebsiteUrl(e.target.value)}
                disabled={loading}
              />
            </div>

            <div className="space-y-2">
              <Label htmlFor="repositoryUrl">Repository URL</Label>
              <div className="flex gap-2">
                <Select value={repositorySource} onValueChange={(v) => setRepositorySource(v as "github" | "gitlab" | "bitbucket")} disabled={loading}>
                  <SelectTrigger className="w-[120px]">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="github">GitHub</SelectItem>
                    <SelectItem value="gitlab">GitLab</SelectItem>
                    <SelectItem value="bitbucket">Bitbucket</SelectItem>
                  </SelectContent>
                </Select>
                <Input
                  id="repositoryUrl"
                  placeholder="https://github.com/user/repo"
                  value={repositoryUrl}
                  onChange={(e) => setRepositoryUrl(e.target.value)}
                  disabled={loading}
                  className="flex-1"
                />
              </div>
            </div>
          </div>

          {/* Packages */}
          <div className="space-y-4 p-4 border rounded-lg">
            <div className="flex items-center justify-between">
              <h3 className="font-semibold text-sm">Packages</h3>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={addPackage}
                disabled={loading}
              >
                <Plus className="h-4 w-4 mr-1" />
                Add Package
              </Button>
            </div>

            {packages.map((pkg, index) => (
              <div key={index} className="space-y-2 p-3 border rounded-md">
                <div className="flex gap-2 items-start">
                  <Input
                    placeholder="Package identifier"
                    value={pkg.identifier}
                    onChange={(e) => updatePackage(index, "identifier", e.target.value)}
                    disabled={loading}
                    className="flex-1"
                  />
                  <Input
                    placeholder="Version"
                    value={pkg.version}
                    onChange={(e) => updatePackage(index, "version", e.target.value)}
                    disabled={loading}
                    className="w-32"
                  />
                  <select
                    value={pkg.registryType}
                    onChange={(e) => updatePackage(index, "registryType", e.target.value)}
                    className="px-3 py-2 border rounded-md bg-background text-foreground border-input focus:outline-none focus:ring-2 focus:ring-ring"
                    disabled={loading}
                  >
                    <option value="npm">npm</option>
                    <option value="pypi">pypi</option>
                    <option value="docker">docker</option>
                  </select>
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    onClick={() => removePackage(index)}
                    disabled={loading}
                  >
                    <Trash2 className="h-4 w-4" />
                  </Button>
                </div>
                <div className="flex gap-3 items-center pl-2">
                  <Label className="text-sm text-muted-foreground">Transport *:</Label>
                  {["stdio", "sse", "streamable-http"].map((transport) => (
                    <label key={transport} className="flex items-center gap-1.5 cursor-pointer">
                      <input
                        type="radio"
                        name={`transport-${index}`}
                        checked={pkg.transport === transport}
                        onChange={() => updatePackage(index, "transport", transport)}
                        disabled={loading}
                        className="border-gray-300"
                      />
                      <span className="text-sm">{transport}</span>
                    </label>
                  ))}
                </div>
              </div>
            ))}

            {packages.length === 0 && (
              <p className="text-sm text-muted-foreground text-center py-2">
                No packages added
              </p>
            )}
          </div>

          {/* Remotes */}
          <div className="space-y-4 p-4 border rounded-lg">
            <div className="flex items-center justify-between">
              <h3 className="font-semibold text-sm">Remotes</h3>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={addRemote}
                disabled={loading}
              >
                <Plus className="h-4 w-4 mr-1" />
                Add Remote
              </Button>
            </div>

            {remotes.map((remote, index) => (
              <div key={index} className="space-y-3 p-3 border rounded-md bg-muted/30">
                <div className="flex gap-2 items-start">
                  <Select
                    value={remote.type}
                    onValueChange={(v) => updateRemote(index, "type", v)}
                    disabled={loading}
                  >
                    <SelectTrigger className="w-48">
                      <SelectValue placeholder="Transport" />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="streamable-http">streamable-http</SelectItem>
                      <SelectItem value="sse">sse</SelectItem>
                    </SelectContent>
                  </Select>
                  <Input
                    placeholder="https://host/mcp"
                    value={remote.url}
                    onChange={(e) => updateRemote(index, "url", e.target.value)}
                    disabled={loading}
                    className="flex-1"
                  />
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    onClick={() => removeRemote(index)}
                    disabled={loading}
                    aria-label="Remove remote"
                  >
                    <Trash2 className="h-4 w-4" />
                  </Button>
                </div>

                {/* Headers for this remote */}
                <div className="space-y-2 pl-2 border-l-2 border-muted">
                  <div className="flex items-center justify-between">
                    <span className="text-xs font-medium text-muted-foreground">
                      HTTP Headers{" "}
                      <span className="font-normal">
                        — declare each header the server expects. Values can be inlined here,
                        or supplied at deploy time via the matching{" "}
                        <code className="text-[11px]">HEADER_&lt;name&gt;</code> environment variable.
                      </span>
                    </span>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      onClick={() => addHeader(index)}
                      disabled={loading}
                    >
                      <Plus className="h-3.5 w-3.5 mr-1" />
                      Add Header
                    </Button>
                  </div>

                  {(remote.headers || []).map((header, hIdx) => (
                    <div key={hIdx} className="space-y-1.5 p-2 rounded border bg-background">
                      <div className="flex gap-2 items-start">
                        <Input
                          placeholder="Authorization"
                          value={header.name}
                          onChange={(e) => updateHeader(index, hIdx, "name", e.target.value)}
                          disabled={loading}
                          className="w-48 font-mono text-xs"
                        />
                        <Input
                          placeholder="Value (optional — leave blank + required to force HEADER_ env at deploy)"
                          value={header.value}
                          onChange={(e) => updateHeader(index, hIdx, "value", e.target.value)}
                          disabled={loading}
                          className="flex-1 text-xs"
                          type={header.isSecret ? "password" : "text"}
                        />
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon"
                          onClick={() => removeHeader(index, hIdx)}
                          disabled={loading}
                          aria-label="Remove header"
                        >
                          <Trash2 className="h-3.5 w-3.5" />
                        </Button>
                      </div>
                      <div className="flex items-center gap-4 pl-1">
                        <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
                          <input
                            type="checkbox"
                            checked={header.isRequired}
                            onChange={(e) => updateHeader(index, hIdx, "isRequired", e.target.checked)}
                            disabled={loading}
                            className="h-3.5 w-3.5"
                          />
                          Required
                        </label>
                        <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
                          <input
                            type="checkbox"
                            checked={header.isSecret}
                            onChange={(e) => updateHeader(index, hIdx, "isSecret", e.target.checked)}
                            disabled={loading}
                            className="h-3.5 w-3.5"
                          />
                          Secret
                        </label>
                        <Input
                          placeholder="Description (optional)"
                          value={header.description}
                          onChange={(e) => updateHeader(index, hIdx, "description", e.target.value)}
                          disabled={loading}
                          className="flex-1 h-7 text-xs"
                        />
                      </div>
                    </div>
                  ))}

                  {(!remote.headers || remote.headers.length === 0) && (
                    <p className="text-xs text-muted-foreground/70 italic pl-1">
                      No headers. If this MCP requires auth, add at least one (e.g. <code className="text-[11px]">Authorization</code>).
                    </p>
                  )}
                </div>
              </div>
            ))}

            {remotes.length === 0 && (
              <p className="text-sm text-muted-foreground text-center py-2">
                No remotes added
              </p>
            )}
          </div>
        </div>

        <div className="flex justify-end gap-2">
          <Button
            variant="outline"
            onClick={() => {
              onOpenChange(false)
              resetForm()
            }}
            disabled={loading}
          >
            Cancel
          </Button>
          <Button
            onClick={handleSubmit}
            disabled={loading || !name.trim() || !version.trim() || !description.trim()}
          >
            {loading && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
            Create Server
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  )
}

