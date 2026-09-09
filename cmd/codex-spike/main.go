// codex-spike verifies an installed CLI without the web UI and can record traffic.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/lepinkainen/hypercode/internal/adapter"
	"github.com/lepinkainen/hypercode/internal/adapter/codex"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	cwd, _ := os.Getwd()
	executable := flag.String("codex", "codex", "Codex executable")
	dir := flag.String("dir", cwd, "Working directory")
	ref := flag.String("resume", "", "Native thread reference to resume")
	prompt := flag.String("prompt", "Reply with exactly: Hypercode connected. Do not use tools.", "Prompt to send")
	record := flag.String("record", "", "Write raw JSONL traffic to this file")
	mode := flag.String("mode", "read-only", "read-only, workspace-write, or full-access")
	decision := flag.String("approve", "cancel", "Decision for approvals: accept, acceptForSession, decline, cancel")
	interrupt := flag.Bool("interrupt", false, "Interrupt after the first text delta")
	model := flag.String("model", "", "Native model id; empty keeps the agent default")
	listModels := flag.Bool("models", false, "List the model catalog and exit")
	imagePath := flag.String("image", "", "Image file to attach")
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
	a := codex.Adapter{Executable: *executable, Trace: trace}
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
	fmt.Println("Thread:", s.Ref(), "model:", s.Model())
	input := adapter.Input{Text: *prompt}
	if *imagePath != "" {
		path, e := filepath.Abs(*imagePath)
		if e != nil {
			return e
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		input.Attachments = []adapter.Attachment{{Name: filepath.Base(path), Path: path, MediaType: http.DetectContentType(data), Size: int64(len(data))}}
	}
	if err = s.Send(ctx, input); err != nil {
		return err
	}
	stopped := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-s.Events():
			if !ok {
				return fmt.Errorf("Codex disconnected")
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
				return fmt.Errorf("question received; use the web UI to answer it")
			case "error":
				return fmt.Errorf("Codex: %s", e.Text)
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
