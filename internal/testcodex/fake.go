// Package testcodex provides a deterministic app-server peer for integration tests.
// It never runs commands or contacts a model.
package testcodex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Run understands [approval], [question], [wait], and [crash] in test prompts.
// Waiting turns advance only when the client replies or interrupts them.
func Run(in io.Reader, out io.Writer) error {
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 65536), 1024*1024)
	enc := json.NewEncoder(out)
	write := func(v any) { _ = enc.Encode(v) }
	event := func(method string, params any) { write(map[string]any{"method": method, "params": params}) }
	ref := "fixture-thread"
	model := ""
	turn := 0
	item := ""
	waiting := ""
	finish := func(text, status string) {
		event("item/agentMessage/delta", map[string]any{"threadId": ref, "itemId": item, "delta": text})
		event("item/completed", map[string]any{"threadId": ref, "item": map[string]any{"id": item, "type": "agentMessage", "text": text}})
		event("turn/completed", map[string]any{"threadId": ref, "turn": map[string]any{"id": fmt.Sprint(turn), "status": status}})
	}
	for scan.Scan() {
		var p struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(scan.Bytes(), &p); err != nil {
			return err
		}
		reply := func(v any) { write(map[string]any{"id": p.ID, "result": v}) }
		switch p.Method {
		case "initialize":
			reply(map[string]string{"userAgent": "fixture/0.153.4"})
		case "initialized":
		case "model/list":
			reply(map[string]any{"data": []any{map[string]any{"id": "fixture-large", "displayName": "Fixture Large", "description": "The default fixture model.", "isDefault": true}, map[string]any{"id": "fixture-small", "displayName": "Fixture Small", "description": "A faster fixture model."}, map[string]any{"id": "fixture-hidden", "displayName": "Hidden", "hidden": true}}})
		case "thread/start", "thread/resume":
			var args struct {
				ThreadID string `json:"threadId"`
				Model    string `json:"model"`
			}
			_ = json.Unmarshal(p.Params, &args)
			if args.ThreadID != "" {
				ref = args.ThreadID
			}
			model = args.Model
			if model == "" {
				model = "fixture-large"
			}
			reply(map[string]any{"thread": map[string]string{"id": ref, "model": model}, "model": model})
		case "turn/start":
			var args struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			_ = json.Unmarshal(p.Params, &args)
			text := ""
			if len(args.Input) > 0 {
				text = args.Input[0].Text
			}
			if strings.Contains(text, "[reject]") {
				write(map[string]any{"id": p.ID, "error": map[string]any{"code": -32602, "message": "Fixture request rejected"}})
				continue
			}
			turn++
			item = fmt.Sprintf("message-%d", turn)
			event("turn/started", map[string]any{"threadId": ref, "turn": map[string]string{"id": fmt.Sprint(turn), "status": "inProgress"}})
			reply(map[string]any{"turn": map[string]string{"id": fmt.Sprint(turn)}})
			event("item/started", map[string]any{"threadId": ref, "item": map[string]string{"id": item, "type": "agentMessage"}})
			switch {
			case strings.Contains(text, "[crash]"):
				return nil
			case strings.Contains(text, "[noise]"):
				// A wrapper script or shim writing plain text to stdout mid-turn.
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintln(out, "npm WARN fixture: plain text on stdout")
				finish("Survived stdout noise.", "completed")
			case strings.Contains(text, "[approval]"):
				waiting = "approval"
				event("item/agentMessage/delta", map[string]any{"threadId": ref, "itemId": item, "delta": "I have inspected the project. Waiting for your approval."})
				event("item/started", map[string]any{"threadId": ref, "item": map[string]string{"id": "tool-" + item, "type": "commandExecution", "command": "printf 'fixture command'"}})
				write(map[string]any{"id": "approval-1", "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": ref, "turnId": fmt.Sprint(turn), "itemId": "tool-" + item, "command": "printf 'fixture command'", "reason": "Fixture approval; no command will execute.", "cwd": "/tmp"}})
			case strings.Contains(text, "[question]"):
				waiting = "question"
				write(map[string]any{"id": 99, "method": "item/tool/requestUserInput", "params": map[string]any{"threadId": ref, "questions": []any{map[string]any{"id": "approach", "header": "Approach", "question": "Which approach should I use?", "options": []any{map[string]string{"label": "Small change", "description": "Keep the patch focused."}, map[string]string{"label": "Refactor", "description": "Restructure the module."}}}, map[string]any{"id": "name", "header": "Name", "question": "What should the feature be called?"}}}})
			case strings.Contains(text, "[wait]"):
				waiting = "wait"
				event("item/agentMessage/delta", map[string]any{"threadId": ref, "itemId": item, "delta": "Working on your task. This turn waits for Stop."})
			default:
				finish("I explored the project.\n\n## Ready to build\n\n- **Codex streaming** is connected (model: "+model+").\n- History stays on this host.\n\n```go\nfmt.Println(\"Hello, Hypercode\")\n```", "completed")
			}
		case "turn/interrupt":
			reply(map[string]any{})
			waiting = ""
			event("turn/completed", map[string]any{"threadId": ref, "turn": map[string]string{"id": fmt.Sprint(turn), "status": "interrupted"}})
		case "":
			switch waiting {
			case "approval":
				event("item/completed", map[string]any{"threadId": ref, "item": map[string]string{"id": "tool-" + item, "type": "commandExecution", "command": "printf 'fixture command'", "aggregatedOutput": "fixture command", "status": "completed"}})
				finish("Approval response received.\n\nThe fixture turn is complete.", "completed")
			case "question":
				finish("Your answers were received.\n\nI will continue with the selected approach.", "completed")
			}
			waiting = ""
		default:
			write(map[string]any{"id": p.ID, "error": map[string]any{"code": -32601, "message": "Unknown method"}})
		}
	}
	return scan.Err()
}
