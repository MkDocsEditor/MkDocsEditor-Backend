package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Pins for agents: the dashboard pins of docs/dashboard.md. See docs/mcp.md.

// maxPinOrder is the most documents of an order_pins
const maxPinOrder = 200

type (
	mcpPin struct {
		Path     string `json:"path"`
		Title    string `json:"title"`
		PinnedBy string `json:"pinnedBy"`
		PinnedAt string `json:"pinnedAt"`
	}

	mcpPins struct {
		Family   []mcpPin `json:"family"`
		Personal []mcpPin `json:"personal"`
	}

	mcpSetPinInput struct {
		Document string `json:"document" jsonschema:"page path relative to the wiki root, like guide/setup.md, or the page id"`
		List     string `json:"list" jsonschema:"family (the same for everyone) or personal (the pins of the account you work for)"`
		Pinned   bool   `json:"pinned" jsonschema:"true: pin the page, at the end of the list; false: unpin it"`
	}

	mcpOrderPinsInput struct {
		List      string   `json:"list" jsonschema:"family or personal"`
		Documents []string `json:"documents" jsonschema:"pinned pages in the new order (paths or ids); the pins not listed follow in their order"`
	}
)

// pinList checks the list of a pin tool: whether it is the family list
func (t *mcpTools) pinList(list string) (bool, error) {
	switch list {
	case "family":
		// the write tools are only there for callers who may write, like the REST endpoints
		return true, nil
	case "personal":
		return false, nil
	}
	return false, errors.New("list must be family or personal")
}

func (t *mcpTools) pins() (*mcpPins, error) {
	pins, err := t.rs.pinsOf(t.caller.user.ID)
	if err != nil {
		return nil, err
	}
	convert := func(in []PinResponse) []mcpPin {
		out := []mcpPin{}
		for _, p := range in {
			out = append(out, mcpPin{Path: p.Path, Title: p.Title, PinnedBy: p.PinnedBy.DisplayName, PinnedAt: p.PinnedAt})
		}
		return out
	}
	return &mcpPins{Family: convert(pins.Family), Personal: convert(pins.Personal)}, nil
}

func (t *mcpTools) listPins(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *mcpPins, error) {
	out, err := t.pins()
	return nil, out, err
}

func (t *mcpTools) setPin(_ context.Context, _ *mcp.CallToolRequest, in mcpSetPinInput) (*mcp.CallToolResult, *mcpPins, error) {
	family, err := t.pinList(in.List)
	if err != nil {
		return nil, nil, err
	}
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	if err = t.allowWrite(); err != nil {
		return nil, nil, err
	}
	if err = t.rs.setPin(t.caller.user, d, family, in.Pinned); err != nil {
		return nil, nil, err
	}
	out, err := t.pins()
	return nil, out, err
}

func (t *mcpTools) orderPins(_ context.Context, _ *mcp.CallToolRequest, in mcpOrderPinsInput) (*mcp.CallToolResult, *mcpPins, error) {
	family, err := t.pinList(in.List)
	if err != nil {
		return nil, nil, err
	}
	if len(in.Documents) == 0 || len(in.Documents) > maxPinOrder {
		return nil, nil, fmt.Errorf("documents must have 1 to %d entries", maxPinOrder)
	}
	paths := make([]string, 0, len(in.Documents))
	for _, ref := range in.Documents {
		var d *wiki.Document
		if d, err = t.document(ref); err != nil {
			return nil, nil, err
		}
		paths = append(paths, d.RelPath)
	}
	if err = t.allowWrite(); err != nil {
		return nil, nil, err
	}
	if err = t.rs.orderPins(t.caller.user, family, paths); err != nil {
		return nil, nil, err
	}
	out, err := t.pins()
	return nil, out, err
}

// registerPinTools adds list_pins, and set_pin and order_pins for callers who may write
func (t *mcpTools) registerPinTools(server *mcp.Server, readOnly *mcp.ToolAnnotations, canWrite bool) {
	mcp.AddTool(server, &mcp.Tool{Name: "list_pins", Annotations: readOnly, Description: "List the pinned pages of the " +
		"dashboard: the family pins, the same for everyone, and the personal pins of the account you work for, each in " +
		"their order. Pinned pages are the ones people use most."}, t.listPins)
	if !canWrite {
		return
	}
	notDestructive := false
	mcp.AddTool(server, &mcp.Tool{Name: "set_pin", Annotations: &mcp.ToolAnnotations{DestructiveHint: &notDestructive},
		Description: "Pin a page to the dashboard (at the end of the list) or unpin it. family pins are seen by " +
			"everyone, personal pins by the account you work for. Only when someone asks for it."}, t.setPin)
	mcp.AddTool(server, &mcp.Tool{Name: "order_pins", Annotations: &mcp.ToolAnnotations{DestructiveHint: &notDestructive},
		Description: "Change the order of the family or personal pins: the listed pages come first, in this order. " +
			"Only when someone asks for it."}, t.orderPins)
}
