package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/client"
	"github.com/cubeplexai/cubepod/pkg/version"
)

func runMCPExchange(t *testing.T, c *client.Client, requests ...string) []jsonRPCResponse {
	t.Helper()
	in := &bytes.Buffer{}
	for _, req := range requests {
		var buf bytes.Buffer
		if err := json.Compact(&buf, []byte(req)); err == nil {
			in.Write(buf.Bytes())
		} else {
			in.WriteString(req)
		}
		in.WriteString("\n")
	}
	out := &bytes.Buffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := ServeMCP(ctx, c, in, out); err != nil {
		t.Fatalf("ServeMCP returned error: %v", err)
	}

	var responses []jsonRPCResponse
	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var resp jsonRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("failed to parse JSON-RPC response %q: %v", string(line), err)
		}
		responses = append(responses, resp)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	return responses
}

func setupMockGateway(t *testing.T) (*httptest.Server, *client.Client) {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		tpl, _ := req["templateID"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.SandboxRecord{
			SandboxID:        "cpod-mcp-test-1",
			TemplateID:       tpl,
			State:            "Running",
			RootfsGeneration: 1,
		})
	})

	mux.HandleFunc("POST /sandboxes/{id}/timeout", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})

	mux.HandleFunc("GET /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		tpl := r.URL.Query().Get("templateID")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]*client.SandboxRecord{
			{
				SandboxID:        "cpod-mcp-test-1",
				TemplateID:       tpl,
				State:            "Running",
				RootfsGeneration: 1,
			},
		})
	})

	mux.HandleFunc("POST /sandboxes/{id}/commands/run", func(w http.ResponseWriter, r *http.Request) {
		var req client.RunCommandRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		exitZero := 0
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.CommandResult{
			ID:       "cmd-1",
			ExitCode: &exitZero,
			Output:   fmt.Sprintf("executed: %s in %s", req.Command, req.Workdir),
			Running:  false,
			PID:      4242,
		})
	})

	mux.HandleFunc("GET /sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "/notfound.txt" {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("mock file contents from sandbox"))
	})

	mux.HandleFunc("PUT /sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(body) == 0 {
			http.Error(w, "empty file body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})

	mux.HandleFunc("POST /sandboxes/{id}/pause", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.SandboxRecord{
			SandboxID:        r.PathValue("id"),
			TemplateID:       "cubeplex-ubuntu24-desktop",
			State:            "Paused",
			RootfsGeneration: 1,
		})
	})

	mux.HandleFunc("POST /sandboxes/{id}/reset-rootfs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.SandboxRecord{
			SandboxID:        r.PathValue("id"),
			TemplateID:       "cubeplex-ubuntu24-desktop",
			State:            "Running",
			RootfsGeneration: 2,
		})
	})

	mux.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "deleted"})
	})

	server := httptest.NewServer(mux)
	c := client.New(server.URL, client.WithTimeout(5*time.Second))
	return server, c
}

func TestMCP_InitializeAndPing(t *testing.T) {
	server, c := setupMockGateway(t)
	defer server.Close()

	responses := runMCPExchange(t, c,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	)

	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(responses))
	}

	// 1. initialize
	initResp := responses[0]
	if string(initResp.ID) != "1" {
		t.Errorf("expected id 1, got %s", string(initResp.ID))
	}
	initResult, ok := initResp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result object in initialize response, got %T", initResp.Result)
	}
	if initResult["protocolVersion"] != "2024-11-05" {
		t.Errorf("expected protocolVersion '2024-11-05', got %v", initResult["protocolVersion"])
	}
	caps, ok := initResult["capabilities"].(map[string]any)
	if !ok || caps["tools"] == nil {
		t.Errorf("expected capabilities.tools object, got %v", caps)
	}
	srvInfo, ok := initResult["serverInfo"].(map[string]any)
	if !ok || srvInfo["name"] != "cubepod-mcp" || srvInfo["version"] != version.Version {
		t.Errorf("unexpected serverInfo: %v", srvInfo)
	}

	// 2. ping
	pingResp := responses[1]
	if string(pingResp.ID) != "2" {
		t.Errorf("expected id 2, got %s", string(pingResp.ID))
	}
	pingResult, ok := pingResp.Result.(map[string]any)
	if !ok || len(pingResult) != 0 {
		t.Errorf("expected empty object for ping, got %v", pingResult)
	}
}

func TestMCP_Notifications(t *testing.T) {
	server, c := setupMockGateway(t)
	defer server.Close()

	responses := runMCPExchange(t, c,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/test","params":{"foo":"bar"}}`,
		`{"jsonrpc":"2.0","id":null,"method":"notifications/null-id"}`,
	)

	if len(responses) != 0 {
		t.Fatalf("expected 0 responses for notifications, got %d: %v", len(responses), responses)
	}
}

func TestMCP_ToolsList(t *testing.T) {
	server, c := setupMockGateway(t)
	defer server.Close()

	responses := runMCPExchange(t, c,
		`{"jsonrpc":"2.0","id":"list-req","method":"tools/list"}`,
	)

	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}

	resp := responses[0]
	if string(resp.ID) != `"list-req"` {
		t.Errorf("expected id \"list-req\", got %s", string(resp.ID))
	}

	resMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %T", resp.Result)
	}

	toolsSlice, ok := resMap["tools"].([]any)
	if !ok {
		// If unmarshalled as typed slice, marshal and unmarshal into []MCPTool
		buf, _ := json.Marshal(resMap["tools"])
		var typedTools []MCPTool
		if err := json.Unmarshal(buf, &typedTools); err != nil {
			t.Fatalf("failed to decode tools list: %v", err)
		}
		toolsSlice = make([]any, len(typedTools))
		for i, v := range typedTools {
			toolsSlice[i] = v
		}
	}

	expectedTools := []string{
		"cubepod_create_sandbox",
		"cubepod_list_sandboxes",
		"cubepod_run_command",
		"cubepod_read_file",
		"cubepod_write_file",
		"cubepod_pause_sandbox",
		"cubepod_reset_rootfs",
		"cubepod_delete_sandbox",
	}

	buf, _ := json.Marshal(resp.Result)
	var toolsObj struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(buf, &toolsObj); err != nil {
		t.Fatalf("failed to unmarshal tools: %v", err)
	}

	if len(toolsObj.Tools) != len(expectedTools) {
		t.Fatalf("expected %d tools, got %d", len(expectedTools), len(toolsObj.Tools))
	}

	toolMap := make(map[string]MCPTool)
	for _, tool := range toolsObj.Tools {
		toolMap[tool.Name] = tool
		if tool.Description == "" {
			t.Errorf("tool %s missing description", tool.Name)
		}
		if tool.InputSchema.Type != "object" {
			t.Errorf("tool %s expected inputSchema type 'object', got %s", tool.Name, tool.InputSchema.Type)
		}
	}

	for _, name := range expectedTools {
		if _, exists := toolMap[name]; !exists {
			t.Errorf("expected tool %s to be listed", name)
		}
	}
}

func TestMCP_ToolsCall_AllTools(t *testing.T) {
	server, c := setupMockGateway(t)
	defer server.Close()

	testCalls := []struct {
		name       string
		request    string
		expectSub  string
		expectErr  bool
		validateFn func(t *testing.T, res CallToolResult)
	}{
		{
			name: "cubepod_create_sandbox",
			request: `{
				"jsonrpc": "2.0",
				"id": 10,
				"method": "tools/call",
				"params": {
					"name": "cubepod_create_sandbox",
					"arguments": {
						"templateID": "cubeplex-ubuntu24-desktop",
						"timeout": 300
					}
				}
			}`,
			expectSub: "cpod-mcp-test-1",
			expectErr: false,
		},
		{
			name: "cubepod_list_sandboxes",
			request: `{
				"jsonrpc": "2.0",
				"id": 11,
				"method": "tools/call",
				"params": {
					"name": "cubepod_list_sandboxes",
					"arguments": {
						"templateID": "cubeplex-ubuntu24-desktop"
					}
				}
			}`,
			expectSub: "cpod-mcp-test-1",
			expectErr: false,
		},
		{
			name: "cubepod_run_command",
			request: `{
				"jsonrpc": "2.0",
				"id": 12,
				"method": "tools/call",
				"params": {
					"name": "cubepod_run_command",
					"arguments": {
						"sandboxID": "cpod-mcp-test-1",
						"command": "echo test",
						"workdir": "/workspace",
						"background": false
					}
				}
			}`,
			expectSub: "executed: echo test in /workspace",
			expectErr: false,
		},
		{
			name: "cubepod_read_file",
			request: `{
				"jsonrpc": "2.0",
				"id": 13,
				"method": "tools/call",
				"params": {
					"name": "cubepod_read_file",
					"arguments": {
						"sandboxID": "cpod-mcp-test-1",
						"path": "/workspace/hello.txt"
					}
				}
			}`,
			expectSub: "mock file contents from sandbox",
			expectErr: false,
		},
		{
			name: "cubepod_write_file",
			request: `{
				"jsonrpc": "2.0",
				"id": 14,
				"method": "tools/call",
				"params": {
					"name": "cubepod_write_file",
					"arguments": {
						"sandboxID": "cpod-mcp-test-1",
						"path": "/workspace/hello.txt",
						"content": "hello world",
						"mode": "0644"
					}
				}
			}`,
			expectSub: "Successfully wrote 11 bytes to /workspace/hello.txt",
			expectErr: false,
		},
		{
			name: "cubepod_pause_sandbox",
			request: `{
				"jsonrpc": "2.0",
				"id": 15,
				"method": "tools/call",
				"params": {
					"name": "cubepod_pause_sandbox",
					"arguments": {
						"sandboxID": "cpod-mcp-test-1"
					}
				}
			}`,
			expectSub: "Paused",
			expectErr: false,
		},
		{
			name: "cubepod_reset_rootfs",
			request: `{
				"jsonrpc": "2.0",
				"id": 16,
				"method": "tools/call",
				"params": {
					"name": "cubepod_reset_rootfs",
					"arguments": {
						"sandboxID": "cpod-mcp-test-1"
					}
				}
			}`,
			expectSub: `"rootfsGeneration": 2`,
			expectErr: false,
		},
		{
			name: "cubepod_delete_sandbox",
			request: `{
				"jsonrpc": "2.0",
				"id": 17,
				"method": "tools/call",
				"params": {
					"name": "cubepod_delete_sandbox",
					"arguments": {
						"sandboxID": "cpod-mcp-test-1",
						"deleteVolumes": true
					}
				}
			}`,
			expectSub: "Successfully deleted sandbox cpod-mcp-test-1",
			expectErr: false,
		},
	}

	for _, tc := range testCalls {
		t.Run(tc.name, func(t *testing.T) {
			responses := runMCPExchange(t, c, tc.request)
			if len(responses) != 1 {
				t.Fatalf("expected 1 response, got %d", len(responses))
			}
			resp := responses[0]
			buf, err := json.Marshal(resp.Result)
			if err != nil {
				t.Fatalf("failed to marshal result: %v", err)
			}
			var toolResult CallToolResult
			if err := json.Unmarshal(buf, &toolResult); err != nil {
				t.Fatalf("failed to unmarshal CallToolResult: %v, raw: %s", err, string(buf))
			}

			if toolResult.IsError != tc.expectErr {
				t.Errorf("expected isError=%v, got %v (text: %v)", tc.expectErr, toolResult.IsError, toolResult.Content)
			}

			if len(toolResult.Content) == 0 {
				t.Fatalf("expected content in CallToolResult, got 0 items")
			}
			if toolResult.Content[0].Type != "text" {
				t.Errorf("expected content[0].type == 'text', got %s", toolResult.Content[0].Type)
			}
			if tc.expectSub != "" && !strings.Contains(toolResult.Content[0].Text, tc.expectSub) {
				t.Errorf("expected content text to contain %q, got %q", tc.expectSub, toolResult.Content[0].Text)
			}
		})
	}
}

func TestMCP_ErrorCases(t *testing.T) {
	server, c := setupMockGateway(t)
	defer server.Close()

	t.Run("UnknownTool", func(t *testing.T) {
		responses := runMCPExchange(t, c, `{
			"jsonrpc": "2.0",
			"id": 101,
			"method": "tools/call",
			"params": {
				"name": "unknown_tool",
				"arguments": {}
			}
		}`)
		if len(responses) != 1 {
			t.Fatalf("expected 1 response, got %d", len(responses))
		}
		buf, _ := json.Marshal(responses[0].Result)
		var tr CallToolResult
		_ = json.Unmarshal(buf, &tr)
		if !tr.IsError {
			t.Errorf("expected isError=true for unknown tool")
		}
		if !strings.Contains(tr.Content[0].Text, "unknown tool") {
			t.Errorf("expected error message to mention unknown tool, got %s", tr.Content[0].Text)
		}
	})

	t.Run("MissingRequiredArguments", func(t *testing.T) {
		responses := runMCPExchange(t, c, `{
			"jsonrpc": "2.0",
			"id": 102,
			"method": "tools/call",
			"params": {
				"name": "cubepod_run_command",
				"arguments": {
					"sandboxID": "cpod-1"
				}
			}
		}`)
		if len(responses) != 1 {
			t.Fatalf("expected 1 response, got %d", len(responses))
		}
		buf, _ := json.Marshal(responses[0].Result)
		var tr CallToolResult
		_ = json.Unmarshal(buf, &tr)
		if !tr.IsError {
			t.Errorf("expected isError=true for missing command argument")
		}
		if !strings.Contains(tr.Content[0].Text, "command is required") {
			t.Errorf("expected error message about required command, got %s", tr.Content[0].Text)
		}
	})

	t.Run("GatewayError", func(t *testing.T) {
		responses := runMCPExchange(t, c, `{
			"jsonrpc": "2.0",
			"id": 103,
			"method": "tools/call",
			"params": {
				"name": "cubepod_read_file",
				"arguments": {
					"sandboxID": "cpod-1",
					"path": "/notfound.txt"
				}
			}
		}`)
		if len(responses) != 1 {
			t.Fatalf("expected 1 response, got %d", len(responses))
		}
		buf, _ := json.Marshal(responses[0].Result)
		var tr CallToolResult
		_ = json.Unmarshal(buf, &tr)
		if !tr.IsError {
			t.Errorf("expected isError=true for 404 from Gateway")
		}
	})

	t.Run("UnknownMethod", func(t *testing.T) {
		responses := runMCPExchange(t, c, `{
			"jsonrpc": "2.0",
			"id": 104,
			"method": "some_random_method"
		}`)
		if len(responses) != 1 {
			t.Fatalf("expected 1 response, got %d", len(responses))
		}
		if responses[0].Error == nil {
			t.Fatalf("expected JSON-RPC error for unknown method, got result: %v", responses[0].Result)
		}
		if responses[0].Error.Code != -32601 {
			t.Errorf("expected error code -32601, got %d", responses[0].Error.Code)
		}
	})

	t.Run("MalformedJSON", func(t *testing.T) {
		responses := runMCPExchange(t, c, `this is definitely not json`)
		if len(responses) != 1 {
			t.Fatalf("expected 1 response, got %d", len(responses))
		}
		if responses[0].Error == nil {
			t.Fatalf("expected JSON-RPC error for malformed json, got result: %v", responses[0].Result)
		}
		if responses[0].Error.Code != -32700 {
			t.Errorf("expected error code -32700, got %d", responses[0].Error.Code)
		}
	})

	t.Run("ContextCancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // immediately cancel
		in := &bytes.Buffer{}
		out := &bytes.Buffer{}
		err := ServeMCP(ctx, c, in, out)
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("expected context canceled error, got %v", err)
		}
	})

	t.Run("LargePayload", func(t *testing.T) {
		largeContent := strings.Repeat("A", 128*1024) // 128 KB
		req := fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 200,
			"method": "tools/call",
			"params": {
				"name": "cubepod_write_file",
				"arguments": {
					"sandboxID": "cpod-mcp-test-1",
					"path": "/workspace/large.txt",
					"content": %q
				}
			}
		}`, largeContent)
		responses := runMCPExchange(t, c, req)
		if len(responses) != 1 {
			t.Fatalf("expected 1 response, got %d", len(responses))
		}
		buf, _ := json.Marshal(responses[0].Result)
		var tr CallToolResult
		_ = json.Unmarshal(buf, &tr)
		if tr.IsError {
			t.Errorf("expected successful write of large payload, got error: %v", tr.Content)
		}
	})
}
