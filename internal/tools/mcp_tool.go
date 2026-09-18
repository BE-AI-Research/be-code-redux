package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/brown-enterprises/be-code/internal/mcp"
)

// MCPTool adapts one MCP server tool into the agent's tool interface.
type MCPTool struct {
	Client *mcp.Client
	Def    mcp.ToolDef
	prefix string // "" → mcp_<server>_<tool>; "ide_" → ide_<tool>
	r      *Registry
}

// AttachMCP registers all of a server's tools under mcp_<server>_<tool>.
func (r *Registry) AttachMCP(client *mcp.Client) []string { return r.AttachMCPPrefixed(client, "") }

// AttachMCPPrefixed registers a server's tools under prefix+<tool>, for
// servers whose tool names should be stable regardless of server name
// (the editor bridge uses "ide_").
func (r *Registry) AttachMCPPrefixed(client *mcp.Client, prefix string) []string {
	r.mcpClients = append(r.mcpClients, client)
	var names []string
	for _, def := range client.Tools() {
		t := &MCPTool{Client: client, Def: def, prefix: prefix, r: r}
		r.AddTool(t)
		names = append(names, t.Name())
	}
	return names
}

func (t *MCPTool) Name() string {
	if t.prefix != "" {
		return t.prefix + t.Def.Name
	}
	return fmt.Sprintf("mcp_%s_%s", t.Client.ServerName, t.Def.Name)
}

func (t *MCPTool) Description() string {
	d := t.Def.Description
	if d == "" {
		d = "tool from MCP server " + t.Client.ServerName
	}
	return d
}

func (t *MCPTool) Schema() json.RawMessage {
	if len(t.Def.InputSchema) > 0 {
		return t.Def.InputSchema
	}
	return schema(`{"type":"object","properties":{}}`)
}

func (t *MCPTool) Run(ctx context.Context, args map[string]any) Result {
	raw, err := json.Marshal(args)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	out, isErr, err := t.Client.CallTool(ctx, t.Def.Name, raw)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	if out == "" {
		out = "(no content returned)"
	}
	return Result{IsError: isErr, Content: truncate(out, t.r.MaxOutput())}
}
