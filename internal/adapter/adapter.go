// Package adapter defines the boundary between Hypercode and installed agents.
package adapter

import "context"

// RejectedError means the agent explicitly refused the request. Other errors,
// including a timeout, leave its acceptance uncertain until events reconcile it.
type RejectedError struct{ Err error }

func (e *RejectedError) Error() string { return e.Err.Error() }
func (e *RejectedError) Unwrap() error { return e.Err }

type PermissionMode string

const (
	ReadOnly       PermissionMode = "read-only"
	WorkspaceWrite PermissionMode = "workspace-write"
	FullAccess     PermissionMode = "full-access"
)

func (p PermissionMode) Valid() bool {
	return p == ReadOnly || p == WorkspaceWrite || p == FullAccess
}

type Adapter interface {
	Open(context.Context, string, PermissionMode) (Session, error)
	Resume(context.Context, string, string, PermissionMode) (Session, error)
}

type Session interface {
	Ref() string
	Send(context.Context, string) error
	Stop(context.Context) error
	Respond(context.Context, string, Answer) error
	Events() <-chan Event
	Close() error
}

type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}
type Question struct {
	ID       string   `json:"id"`
	Header   string   `json:"header"`
	Question string   `json:"question"`
	Options  []Option `json:"options"`
	IsSecret bool     `json:"isSecret"`
}
type Prompt struct {
	RequestID string     `json:"request_id"`
	Title     string     `json:"title"`
	Detail    string     `json:"detail"`
	Decisions []string   `json:"decisions,omitempty"`
	Questions []Question `json:"questions,omitempty"`
}
type Answer struct {
	Decision string
	Answers  map[string][]string
}
type Event struct {
	Kind   string
	ID     string
	Text   string
	Status string
	Prompt *Prompt
}
