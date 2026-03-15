package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/arkonis-dev/ark-operator/internal/agent/config"
)

const contentTypeText = "text"

// Tool is the internal representation of a tool discovered from an MCP server.
type Tool struct {
	// Name is prefixed: "<server_name>__<original_tool_name>"
	Name        string
	Description string
	InputSchema json.RawMessage
	ServerURL   string
	// OriginalName is the tool name as registered on the MCP server (without prefix).
	OriginalName string
}

// Manager connects to MCP servers and provides tool discovery and execution.
type Manager struct {
	tools []Tool
}

// NewManager connects to all configured MCP servers and discovers their tools.
func NewManager(servers []config.MCPServerConfig) (*Manager, error) {
	m := &Manager{}
	for _, s := range servers {
		tools, err := discoverTools(s)
		if err != nil {
			return nil, fmt.Errorf("failed to discover tools from MCP server %q: %w", s.Name, err)
		}
		m.tools = append(m.tools, tools...)
	}
	return m, nil
}

// discoverTools calls the MCP server's tools/list endpoint.
func discoverTools(server config.MCPServerConfig) ([]Tool, error) {
	resp, err := http.Post(server.URL+"/tools/list", "application/json", nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tools/list returned status %d", resp.StatusCode)
	}

	var result struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding tools/list response: %w", err)
	}

	tools := make([]Tool, 0, len(result.Tools))
	for _, t := range result.Tools {
		tools = append(tools, Tool{
			// Prefix with server name to avoid collisions across multiple MCP servers.
			Name:         fmt.Sprintf("%s__%s", server.Name, t.Name),
			OriginalName: t.Name,
			Description:  t.Description,
			InputSchema:  t.InputSchema,
			ServerURL:    server.URL,
		})
	}
	return tools, nil
}

// Tools returns the full list of discovered MCP tools.
func (m *Manager) Tools() []Tool {
	return m.tools
}

// CallTool executes a tool on its MCP server and returns the text result.
func (m *Manager) CallTool(ctx context.Context, toolName string, input json.RawMessage) (string, error) {
	for _, t := range m.tools {
		if t.Name != toolName {
			continue
		}

		body, err := json.Marshal(map[string]any{
			"name":      t.OriginalName,
			"arguments": input,
		})
		if err != nil {
			return "", fmt.Errorf("marshalling tool call: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.ServerURL+"/tools/call",
			bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("tools/call request failed: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		var result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return "", fmt.Errorf("decoding tools/call response: %w", err)
		}

		for _, c := range result.Content {
			if c.Type == contentTypeText {
				return c.Text, nil
			}
		}
		return "", nil
	}
	return "", fmt.Errorf("tool %q not found in any MCP server", toolName)
}

// Close is a no-op for HTTP-based MCP servers. Reserved for future SSE cleanup.
func (m *Manager) Close() {}
