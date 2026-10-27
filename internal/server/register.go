package server

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"arkkvm-mcp/internal/toolerr"
)

// register adds one tool with a raw JSON input schema and the handler doing
// its own validation. A handler error is wrapped into an isError result whose
// text content is the bare message.
func register(mcpServer *mcp.Server, name, description, inputSchema string, handler mcp.ToolHandler) {
	mcpServer.AddTool(&mcp.Tool{
		Name:        name,
		Description: description,
		InputSchema: json.RawMessage(inputSchema),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result, err := handler(ctx, req)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				IsError: true,
			}, nil
		}
		return result, nil
	})
}

// resultCallResult builds the JSON-mirror success envelope: compact JSON as
// text content mirrored in structuredContent.
func resultCallResult(payload any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(data)}},
		StructuredContent: json.RawMessage(data),
	}, nil
}

// imageCallResult builds image + text content with a structured mirror.
func imageCallResult(images [][]byte, payload any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	contents := make([]mcp.Content, 0, len(images)+1)
	for _, image := range images {
		contents = append(contents, &mcp.ImageContent{Data: image, MIMEType: "image/png"})
	}
	contents = append(contents, &mcp.TextContent{Text: string(data)})
	return &mcp.CallToolResult{
		Content:           contents,
		StructuredContent: json.RawMessage(data),
	}, nil
}

// errResult builds the isError result carrying the structured payload.
func errResult(res *toolerr.ToolResult) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: res.ContentText}},
		StructuredContent: json.RawMessage(res.ContentText),
		IsError:           true,
	}, nil
}

// structured wraps a handler: successes go through the JSON-mirror envelope;
// errors are classified into stable codes.
func structured(fn func() (any, error)) (*mcp.CallToolResult, error) {
	payload, err := fn()
	if err != nil {
		return errResult(toolerr.ClassifyToolError(err))
	}
	return resultCallResult(payload)
}

// schemaless is the empty input schema shared by the no-argument tools.
const schemaless = `{"type":"object","properties":{},"additionalProperties":false}`

// Aliases keep handler signatures terse.
type (
	callToolRequest = mcp.CallToolRequest
	callToolResult  = mcp.CallToolResult
)
