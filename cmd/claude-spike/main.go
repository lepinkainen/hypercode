// claude-spike verifies an installed CLI without the web UI and can record traffic.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/lepinkainen/hypercode/internal/adapter"
	"github.com/lepinkainen/hypercode/internal/adapter/claude"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	cwd, _ := os.Getwd()
	executable := flag.String("claude", "claude", "Claude Code executable")
	dir := flag.String("dir", cwd, "Working directory")
	ref := flag.String("resume", "", "Native session reference to resume")
	prompt := flag.String("prompt", "Reply with exactly: Hypercode connected. Do not use tools.", "Prompt to send")
	record := flag.String("record", "", "Write raw JSONL traffic to this file")
	mode := flag.String("mode", "read-only", "read-only, workspace-write, or full-access")
	decision := flag.String("approve", "cancel", "Decision for approvals: accept, acceptForSession, decline, cancel")
	interrupt := flag.Bool("interrupt", false, "Interrupt after the first text delta")
	model := flag.String("model", "", "Native model id; empty keeps the agent default")
	listModels := flag.Bool("models", false, "List the model catalog and exit")
	flag.Parse()
	var trace io.Writer
	if *record != "" {
		f, err := os.OpenFile(*record, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		trace = f
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	a := claude.Adapter{Executable: *executable, Trace: trace}
	if *listModels {
		models, err := a.Models(ctx)
		if err != nil {
			return err
		}
		for _, m := range models {
			marker := " "
			if m.Default {
				marker = "*"
			}
			fmt.Printf("%s %-28s %-22s %s\n", marker, m.ID, m.Name, m.Description)
		}
		return nil
	}
	var s adapter.Session
	var err error
	if *ref == "" {
		s, err = a.Open(ctx, *dir, adapter.PermissionMode(*mode), *model)
	} else {
		s, err = a.Resume(ctx, *dir, *ref, adapter.PermissionMode(*mode), *model)
	}
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Println("Session:", s.Ref(), "model:", s.Model())
	if err = s.Send(ctx, *prompt); err != nil {
		return err
	}
	stopped := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-s.Events():
			if !ok {
				return fmt.Errorf("Claude Code disconnected")
			}
			switch e.Kind {
			case "text_delta":
				fmt.Print(e.Text)
				if *interrupt && !stopped {
					stopped = true
					if err = s.Stop(ctx); err != nil {
						return err
					}
				}
			case "approval_requested":
				fmt.Printf("\nApproval: %s\n%s\n", e.Prompt.Title, e.Prompt.Detail)
				if err = s.Respond(ctx, e.ID, adapter.Answer{Decision: *decision}); err != nil {
					return err
				}
			case "question_asked":
				answers := map[string][]string{}
				for _, q := range e.Prompt.Questions {
					answer := "Hypercode spike answer"
					if len(q.Options) > 0 {
						answer = q.Options[0].Label
					}
					fmt.Printf("\nQuestion: %s -> %s\n", q.Question, answer)
					answers[q.ID] = []string{answer}
				}
				if err = s.Respond(ctx, e.ID, adapter.Answer{Answers: answers}); err != nil {
					return err
				}
			case "tool_started", "tool_done":
				fmt.Printf("\n[%s] %s\n", e.Kind, e.Text)
			case "status":
				fmt.Printf("\n[status] %s %s\n", e.Status, e.Text)
			case "error":
				return fmt.Errorf("Claude Code: %s", e.Text)
			case "turn_done":
				fmt.Printf("\nTurn: %s\n", e.Status)
				if e.Status == "failed" {
					return fmt.Errorf("turn failed: %s", e.Text)
				}
				return nil
			}
		}
	}
}
