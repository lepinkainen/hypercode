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

// Model is one entry of a harness's selectable model catalog. An empty ID
// means the harness's own default.
type Model struct {
	ID          string
	Name        string
	Description string
	Default     bool
}

type Adapter interface {
	// Open starts a fresh native session in dir. An empty model keeps the
	// harness default.
	Open(ctx context.Context, dir string, mode PermissionMode, model string) (Session, error)
	// Resume reattaches to a stored native session, cold if necessary.
	Resume(ctx context.Context, dir, ref string, mode PermissionMode, model string) (Session, error)
	// Models lists the catalog the installed CLI offers this account.
	Models(context.Context) ([]Model, error)
}

type Session interface {
	Ref() string
	// Model is the native model id the session actually runs with, resolved
	// from the harness default when Open received an empty model.
	Model() string
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
