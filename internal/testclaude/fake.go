// Package testclaude provides a deterministic Claude Code stream-json peer for
// integration tests. It never runs commands or contacts a model.
package testclaude

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// SessionFromArgs returns the --session-id or --resume value from a command
// line, mirroring what the real CLI would echo in its init frame.
func SessionFromArgs(args []string) string {
	return flagValue(args, "--session-id", "--resume", "fixture-session")
}

// ModelFromArgs returns the --model value from a command line.
func ModelFromArgs(args []string) string { return flagValue(args, "--model", "", "") }

func flagValue(args []string, a, b, fallback string) string {
	for i, arg := range args {
		if (arg == a || (b != "" && arg == b)) && i+1 < len(args) {
			return args[i+1]
		}
	}
	return fallback
}

// Run understands [approval], [question], [wait], [cancel], [noise], and
// [crash] in test prompts. Waiting turns advance only when the client replies
// or interrupts them.
func Run(in io.Reader, out io.Writer, sessionID, model string) error {
	if model == "" {
		model = "fixture-model"
	}
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 65536), 40*1024*1024)
	enc := json.NewEncoder(out)
	write := func(v any) { _ = enc.Encode(v) }
	turn := 0
	waiting := ""
	tool := ""
	stream := func(event any) {
		write(map[string]any{"type": "stream_event", "event": event, "parent_tool_use_id": nil, "session_id": sessionID})
	}
	say := func(text string) {
		id := fmt.Sprintf("message-%d", turn)
		stream(map[string]any{"type": "message_start", "message": map[string]any{"id": id, "role": "assistant", "content": []any{}}})
		stream(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		stream(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
		write(map[string]any{"type": "assistant", "message": map[string]any{"id": id, "role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}, "parent_tool_use_id": nil, "session_id": sessionID})
		stream(map[string]any{"type": "content_block_stop", "index": 0})
		stream(map[string]any{"type": "message_stop"})
	}
	result := func(reason string) {
		write(map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": turn, "result": "", "terminal_reason": reason, "session_id": sessionID, "permission_denials": []any{}})
	}
	finish := func(text string) {
		say(text)
		result("completed")
	}
	toolResult := func(content string, isError bool) {
		write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": tool, "content": content, "is_error": isError}}}, "parent_tool_use_id": nil, "session_id": sessionID})
	}
	for scan.Scan() {
		var p struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
			Response struct {
				RequestID string `json:"request_id"`
				Response  struct {
					Behavior string          `json:"behavior"`
					Updated  json.RawMessage `json:"updatedInput"`
				} `json:"response"`
			} `json:"response"`
			Message struct {
				Content []struct {
					Text   string `json:"text"`
					Type   string `json:"type"`
					Source struct {
						Type      string `json:"type"`
						MediaType string `json:"media_type"`
						Data      string `json:"data"`
					} `json:"source"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scan.Bytes(), &p); err != nil {
			return err
		}
		switch p.Type {
		case "control_request":
			switch p.Request.Subtype {
			case "initialize":
				write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": p.RequestID, "response": map[string]any{"commands": []any{}, "models": []any{map[string]any{"value": "default", "resolvedModel": "fixture-model", "displayName": "Default (recommended)", "description": "Fixture default model"}, map[string]any{"value": "fixture-fast", "resolvedModel": "fixture-fast-2", "displayName": "Fixture Fast", "description": "A faster fixture model"}}}}})
			case "get_settings":
				write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": p.RequestID, "response": map[string]any{"effective": map[string]any{"model": model}}}})
			case "interrupt":
				write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": p.RequestID, "response": map[string]any{"still_queued": []any{}}}})
				if waiting != "" {
					waiting = ""
					result("aborted_streaming")
				}
			default:
				write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": p.RequestID, "error": "Unknown control request"}})
			}
		case "user":
			text := ""
			images, imageBytes := 0, 0
			for _, input := range p.Message.Content {
				if input.Type == "text" {
					text += input.Text
				}
				if input.Type == "image" {
					if input.Source.Type != "base64" || !strings.HasPrefix(input.Source.MediaType, "image/") {
						return fmt.Errorf("invalid image source")
					}
					b, err := base64.StdEncoding.DecodeString(input.Source.Data)
					if err != nil {
						return err
					}
					images++
					imageBytes += len(b)
				}
			}
			turn++
			tool = fmt.Sprintf("tool-%d", turn)
			write(map[string]any{"type": "system", "subtype": "init", "session_id": sessionID, "model": model, "permissionMode": "default", "cwd": "/tmp/project", "tools": []string{"Bash", "AskUserQuestion"}})
			switch {
			case strings.Contains(text, "[crash]"):
				return nil
			case strings.Contains(text, "[noise]"):
				// A wrapper script or shim writing plain text to stdout mid-turn.
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintln(out, "npm WARN fixture: plain text on stdout")
				finish("Survived stdout noise.")
			case strings.Contains(text, "[approval]"), strings.Contains(text, "[cancel]"):
				waiting = "approval"
				say("I have inspected the project. Waiting for your approval.")
				write(map[string]any{"type": "assistant", "message": map[string]any{"id": "message-tool", "role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": tool, "name": "Bash", "input": map[string]any{"command": "printf 'fixture command'", "description": "Fixture approval; no command will execute."}}}}, "parent_tool_use_id": nil, "session_id": sessionID})
				write(map[string]any{"type": "control_request", "request_id": "approval-" + tool, "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "printf 'fixture command'", "description": "Fixture approval; no command will execute."}, "tool_use_id": tool, "description": "Fixture approval; no command will execute.", "permission_suggestions": []any{map[string]any{"type": "addRules", "rules": []any{map[string]any{"toolName": "Bash", "ruleContent": "printf 'fixture command'"}}, "behavior": "allow", "destination": "localSettings"}}}})
				if strings.Contains(text, "[cancel]") {
					waiting = ""
					write(map[string]any{"type": "control_cancel_request", "request_id": "approval-" + tool})
					toolResult("Request withdrawn.", true)
					finish("The approval was withdrawn.")
				}
			case strings.Contains(text, "[question]"):
				waiting = "question"
				write(map[string]any{"type": "control_request", "request_id": "question-" + tool, "request": map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "input": map[string]any{"questions": []any{map[string]any{"question": "Which approach should I use?", "header": "Approach", "options": []any{map[string]string{"label": "Small change", "description": "Keep the patch focused."}, map[string]string{"label": "Refactor", "description": "Restructure the module."}}, "multiSelect": false}, map[string]any{"question": "What should the feature be called?", "header": "Name", "options": []any{map[string]string{"label": "Alpha", "description": ""}, map[string]string{"label": "Beta", "description": ""}}, "multiSelect": false}}}, "tool_use_id": tool, "requires_user_interaction": true}})
			case strings.Contains(text, "[wait]"):
				waiting = "wait"
				stream(map[string]any{"type": "message_start", "message": map[string]any{"id": fmt.Sprintf("message-%d", turn), "role": "assistant", "content": []any{}}})
				stream(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Working on your task. This turn waits for Stop."}})
			case images > 0:
				finish(fmt.Sprintf("Received %d images (%d bytes). %s", images, imageBytes, text))
			default:
				finish("I explored the project.\n\n## Ready to build\n\n- **Claude Code streaming** is connected (model: " + model + ").\n- History stays on this host.\n\n```go\nfmt.Println(\"Hello, Hypercode\")\n```")
			}
		case "control_response":
			switch waiting {
			case "approval":
				if p.Response.Response.Behavior == "allow" {
					toolResult("fixture command", false)
				} else {
					toolResult("User declined tool execution.", true)
				}
				finish("Approval response received.\n\nThe fixture turn is complete.")
			case "question":
				finish("Your answers were received: " + string(p.Response.Response.Updated) + "\n\nI will continue with the selected approach.")
			}
			waiting = ""
		}
	}
	return scan.Err()
}
