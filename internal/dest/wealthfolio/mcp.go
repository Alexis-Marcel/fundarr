package wealthfolio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// A minimal MCP client (Streamable HTTP transport, JSON-RPC 2.0): enough to
// initialize a session and call tools with a Personal Access Token.

type mcpClient struct {
	url   string
	token string
	http  *http.Client

	session string
	nextID  int
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (c *mcpClient) post(ctx context.Context, method string, params any, notify bool) (json.RawMessage, error) {
	body := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		body["params"] = params
	}
	id := 0
	if !notify {
		c.nextID++
		id = c.nextID
		body["id"] = id
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, errors.New("Wealthfolio refused the access token (check it exists, is not revoked, and has the activities scopes)")
	case resp.StatusCode == http.StatusNotFound:
		return nil, errors.New("no MCP endpoint at this URL: set WF_MCP_ENABLED=true on the Wealthfolio server")
	case resp.StatusCode >= 300:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("Wealthfolio %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if notify {
		io.Copy(io.Discard, resp.Body)
		return nil, nil
	}
	var r *rpcResponse
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			var cand rpcResponse
			if json.Unmarshal([]byte(data), &cand) == nil && cand.ID == id {
				r = &cand
				break
			}
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	} else {
		var cand rpcResponse
		if err := json.NewDecoder(resp.Body).Decode(&cand); err != nil {
			return nil, err
		}
		r = &cand
	}
	if r == nil {
		return nil, fmt.Errorf("no response to %s", method)
	}
	if r.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, r.Error.Message)
	}
	return r.Result, nil
}

func (c *mcpClient) init(ctx context.Context) error {
	if c.session != "" {
		return nil
	}
	_, err := c.post(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "fundarr", "version": "1"},
	}, false)
	if err != nil {
		return err
	}
	_, err = c.post(ctx, "notifications/initialized", nil, true)
	return err
}

// call runs a tool and decodes its structured result into out.
func (c *mcpClient) call(ctx context.Context, tool string, args any, out any) error {
	if err := c.init(ctx); err != nil {
		return err
	}
	raw, err := c.post(ctx, "tools/call", map[string]any{"name": tool, "arguments": args}, false)
	if err != nil {
		return err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	text := ""
	for _, c := range res.Content {
		text += c.Text
	}
	if res.IsError {
		if strings.Contains(text, "scope") {
			return fmt.Errorf("%s: %s (give the token the accounts:read, activities:read, activities:draft and activities:write scopes)", tool, text)
		}
		return fmt.Errorf("%s: %s", tool, text)
	}
	if out == nil {
		return nil
	}
	if len(res.Structured) > 0 && string(res.Structured) != "null" {
		return json.Unmarshal(res.Structured, out)
	}
	return json.Unmarshal([]byte(text), out)
}
