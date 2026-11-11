// Package main implements the cubepod command-line interface and native MCP server.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/cubeplexai/cubepod/pkg/client"
	"github.com/cubeplexai/cubepod/pkg/version"
)

// MCPTool defines the metadata and input schema for a Model Context Protocol tool.
type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema MCPInputSchema `json:"inputSchema"`
}

// MCPInputSchema represents the JSON Schema for tool parameters.
type MCPInputSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]MCPProperty `json:"properties"`
	Required   []string               `json:"required,omitempty"`
}

// MCPProperty describes a single parameter property in MCPInputSchema.
type MCPProperty struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

// ToolCallParams represents incoming arguments for tools/call.
type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// MCPContent describes a content payload in CallToolResult.
type MCPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CallToolResult represents the standardized MCP tool call execution response.
type CallToolResult struct {
	Content []MCPContent `json:"content"`
	IsError bool         `json:"isError"`
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

var mcpTools = []MCPTool{
	{
		Name:        "cubepod_create_sandbox",
		Description: "Provision a new persistent MicroVM execution sandbox",
		InputSchema: MCPInputSchema{
			Type: "object",
			Properties: map[string]MCPProperty{
				"templateID": {
					Type:        "string",
					Description: "Template ID for the sandbox base image (default: cubeplex-ubuntu24-desktop)",
				},
				"timeout": {
					Type:        "integer",
					Description: "Optional idle timeout in seconds before auto-pause",
				},
			},
		},
	},
	{
		Name:        "cubepod_list_sandboxes",
		Description: "List active and paused CubePod sandboxes, optionally filtered by template ID",
		InputSchema: MCPInputSchema{
			Type: "object",
			Properties: map[string]MCPProperty{
				"templateID": {
					Type:        "string",
					Description: "Optional template ID filter",
				},
			},
		},
	},
	{
		Name:        "cubepod_run_command",
		Description: "Execute a shell command inside a CubePod sandbox",
		InputSchema: MCPInputSchema{
			Type: "object",
			Properties: map[string]MCPProperty{
				"sandboxID": {
					Type:        "string",
					Description: "Target sandbox ID",
				},
				"command": {
					Type:        "string",
					Description: "Shell command string to execute",
				},
				"workdir": {
					Type:        "string",
					Description: "Optional working directory inside the sandbox (default: /workspace)",
				},
				"background": {
					Type:        "boolean",
					Description: "Whether to run the command in the background",
				},
			},
			Required: []string{"sandboxID", "command"},
		},
	},
	{
		Name:        "cubepod_read_file",
		Description: "Read file contents from inside a CubePod sandbox",
		InputSchema: MCPInputSchema{
			Type: "object",
			Properties: map[string]MCPProperty{
				"sandboxID": {
					Type:        "string",
					Description: "Target sandbox ID",
				},
				"path": {
					Type:        "string",
					Description: "Absolute path of the file inside the sandbox",
				},
			},
			Required: []string{"sandboxID", "path"},
		},
	},
	{
		Name:        "cubepod_write_file",
		Description: "Write content to a file inside a CubePod sandbox",
		InputSchema: MCPInputSchema{
			Type: "object",
			Properties: map[string]MCPProperty{
				"sandboxID": {
					Type:        "string",
					Description: "Target sandbox ID",
				},
				"path": {
					Type:        "string",
					Description: "Absolute destination path inside the sandbox",
				},
				"content": {
					Type:        "string",
					Description: "Text or binary content to write",
				},
				"mode": {
					Type:        "string",
					Description: "File permissions mode (default: 0644)",
				},
			},
			Required: []string{"sandboxID", "path", "content"},
		},
	},
	{
		Name:        "cubepod_pause_sandbox",
		Description: "Pause a running sandbox while retaining persistent volumes",
		InputSchema: MCPInputSchema{
			Type: "object",
			Properties: map[string]MCPProperty{
				"sandboxID": {
					Type:        "string",
					Description: "Target sandbox ID to pause",
				},
			},
			Required: []string{"sandboxID"},
		},
	},
	{
		Name:        "cubepod_reset_rootfs",
		Description: "Reset the persistent RootFS Upper layer of a sandbox while preserving /workspace",
		InputSchema: MCPInputSchema{
			Type: "object",
			Properties: map[string]MCPProperty{
				"sandboxID": {
					Type:        "string",
					Description: "Target sandbox ID",
				},
			},
			Required: []string{"sandboxID"},
		},
	},
	{
		Name:        "cubepod_delete_sandbox",
		Description: "Permanently terminate and delete a CubePod sandbox",
		InputSchema: MCPInputSchema{
			Type: "object",
			Properties: map[string]MCPProperty{
				"sandboxID": {
					Type:        "string",
					Description: "Target sandbox ID to delete",
				},
				"deleteVolumes": {
					Type:        "boolean",
					Description: "Whether to also delete persistent volumes (default: false)",
				},
			},
			Required: []string{"sandboxID"},
		},
	},
}

// ServeMCP serves Model Context Protocol JSON-RPC 2.0 requests over an in/out stdio stream.
func ServeMCP(ctx context.Context, c *client.Client, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	var writeMu sync.Mutex

	sendResponse := func(resp jsonRPCResponse) error {
		data, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		data = append(data, '\n')
		_, err = out.Write(data)
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := reader.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			if err == io.EOF {
				return nil
			}
			return err
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			if err == io.EOF {
				return nil
			}
			continue
		}

		var req jsonRPCRequest
		if unmarshalErr := json.Unmarshal(line, &req); unmarshalErr != nil {
			_ = sendResponse(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error: &jsonRPCError{
					Code:    -32700,
					Message: "parse error: invalid JSON-RPC 2.0 message",
				},
			})
			continue
		}

		// Method 'notifications/initialized' or any notification without 'id': no response required.
		if req.Method == "notifications/initialized" || len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}

		resp := jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
		}

		switch req.Method {
		case "initialize":
			resp.Result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "cubepod-mcp",
					"version": version.Version,
				},
			}

		case "ping":
			resp.Result = map[string]any{}

		case "tools/list":
			resp.Result = map[string]any{
				"tools": mcpTools,
			}

		case "tools/call":
			resp.Result = handleToolCall(ctx, c, req.Params)

		default:
			resp.Error = &jsonRPCError{
				Code:    -32601,
				Message: fmt.Sprintf("method not found: %s", req.Method),
			}
		}

		if sendErr := sendResponse(resp); sendErr != nil {
			return sendErr
		}
	}
}

func handleToolCall(ctx context.Context, c *client.Client, rawParams json.RawMessage) *CallToolResult {
	var p ToolCallParams
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return &CallToolResult{
			Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("invalid tool call params: %v", err)}},
			IsError: true,
		}
	}

	argsJSON := p.Arguments
	if len(argsJSON) == 0 || string(argsJSON) == "null" {
		argsJSON = []byte("{}")
	}

	switch p.Name {
	case "cubepod_create_sandbox":
		var args struct {
			TemplateID string `json:"templateID"`
			Timeout    int    `json:"timeout"`
		}
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("invalid arguments: %v", err)}}, IsError: true}
		}
		tpl := args.TemplateID
		if tpl == "" {
			tpl = "cubeplex-ubuntu24-desktop"
		}
		sb, err := c.CreateSandbox(ctx, client.CreateSandboxRequest{
			TemplateID: tpl,
		})
		if err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		if args.Timeout > 0 {
			if err := c.SetTimeout(ctx, sb.SandboxID, args.Timeout); err != nil {
				return &CallToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("created sandbox %s but failed to set timeout: %v", sb.SandboxID, err)}}, IsError: true}
			}
		}
		out, err := json.MarshalIndent(sb, "", "  ")
		if err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		return &CallToolResult{Content: []MCPContent{{Type: "text", Text: string(out)}}, IsError: false}

	case "cubepod_list_sandboxes":
		var args struct {
			TemplateID string `json:"templateID"`
		}
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("invalid arguments: %v", err)}}, IsError: true}
		}
		list, err := c.ListSandboxes(ctx, args.TemplateID)
		if err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		out, err := json.MarshalIndent(list, "", "  ")
		if err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		return &CallToolResult{Content: []MCPContent{{Type: "text", Text: string(out)}}, IsError: false}

	case "cubepod_run_command":
		var args struct {
			SandboxID  string `json:"sandboxID"`
			Command    string `json:"command"`
			Workdir    string `json:"workdir"`
			Background bool   `json:"background"`
		}
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("invalid arguments: %v", err)}}, IsError: true}
		}
		if args.SandboxID == "" {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: "sandboxID is required"}}, IsError: true}
		}
		if args.Command == "" {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: "command is required"}}, IsError: true}
		}
		res, err := c.RunCommand(ctx, args.SandboxID, client.RunCommandRequest{
			Command:    args.Command,
			Workdir:    args.Workdir,
			Background: args.Background,
		})
		if err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		out, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		return &CallToolResult{Content: []MCPContent{{Type: "text", Text: string(out)}}, IsError: false}

	case "cubepod_read_file":
		var args struct {
			SandboxID string `json:"sandboxID"`
			Path      string `json:"path"`
		}
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("invalid arguments: %v", err)}}, IsError: true}
		}
		if args.SandboxID == "" {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: "sandboxID is required"}}, IsError: true}
		}
		if args.Path == "" {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: "path is required"}}, IsError: true}
		}
		data, err := c.ReadFile(ctx, args.SandboxID, args.Path)
		if err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		return &CallToolResult{Content: []MCPContent{{Type: "text", Text: string(data)}}, IsError: false}

	case "cubepod_write_file":
		var args struct {
			SandboxID string `json:"sandboxID"`
			Path      string `json:"path"`
			Content   string `json:"content"`
			Mode      string `json:"mode"`
		}
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("invalid arguments: %v", err)}}, IsError: true}
		}
		if args.SandboxID == "" {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: "sandboxID is required"}}, IsError: true}
		}
		if args.Path == "" {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: "path is required"}}, IsError: true}
		}
		mode := args.Mode
		if mode == "" {
			mode = "0644"
		}
		if err := c.WriteFile(ctx, args.SandboxID, args.Path, []byte(args.Content), mode); err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		return &CallToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("Successfully wrote %d bytes to %s", len(args.Content), args.Path)}}, IsError: false}

	case "cubepod_pause_sandbox":
		var args struct {
			SandboxID string `json:"sandboxID"`
		}
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("invalid arguments: %v", err)}}, IsError: true}
		}
		if args.SandboxID == "" {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: "sandboxID is required"}}, IsError: true}
		}
		sb, err := c.PauseSandbox(ctx, args.SandboxID)
		if err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		out, err := json.MarshalIndent(sb, "", "  ")
		if err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		return &CallToolResult{Content: []MCPContent{{Type: "text", Text: string(out)}}, IsError: false}

	case "cubepod_reset_rootfs":
		var args struct {
			SandboxID string `json:"sandboxID"`
		}
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("invalid arguments: %v", err)}}, IsError: true}
		}
		if args.SandboxID == "" {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: "sandboxID is required"}}, IsError: true}
		}
		sb, err := c.ResetRootFS(ctx, args.SandboxID)
		if err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		out, err := json.MarshalIndent(sb, "", "  ")
		if err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		return &CallToolResult{Content: []MCPContent{{Type: "text", Text: string(out)}}, IsError: false}

	case "cubepod_delete_sandbox":
		var args struct {
			SandboxID     string `json:"sandboxID"`
			DeleteVolumes bool   `json:"deleteVolumes"`
		}
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("invalid arguments: %v", err)}}, IsError: true}
		}
		if args.SandboxID == "" {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: "sandboxID is required"}}, IsError: true}
		}
		if err := c.DeleteSandboxWithOptions(ctx, args.SandboxID, args.DeleteVolumes); err != nil {
			return &CallToolResult{Content: []MCPContent{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		return &CallToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("Successfully deleted sandbox %s", args.SandboxID)}}, IsError: false}

	default:
		return &CallToolResult{
			Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("unknown tool: %s", p.Name)}},
			IsError: true,
		}
	}
}
