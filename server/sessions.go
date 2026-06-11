package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"

	gendb "github.com/maikdotfi/metaharness/db"
)

// workspaceRoot is where each local machine's workspace directory is created.
const workspaceRoot = "workspaces"

// --- Huma input/output types ---

type CreateSessionInput struct {
	Body struct {
		Prompt string `json:"prompt" required:"true" doc:"The task to give the agent"`
	}
}

type SessionBody struct {
	ID        int64  `json:"id" doc:"Session ID"`
	Prompt    string `json:"prompt" doc:"The prompt the session ran"`
	Workdir   string `json:"workdir" doc:"The machine workspace directory"`
	Status    string `json:"status" doc:"running | done | error"`
	FinalText string `json:"final_text" doc:"The agent's final message"`
	Error     string `json:"error,omitempty" doc:"Error, if the run failed"`
	CreatedAt string `json:"created_at" doc:"Creation timestamp"`
}

type SessionOutput struct {
	Body SessionBody
}

type GetSessionInput struct {
	ID int64 `path:"id" doc:"Session ID" example:"1"`
}

type EventBody struct {
	Seq     int64           `json:"seq" doc:"Order within the session"`
	Role    string          `json:"role" doc:"user | assistant | tool | system"`
	Message json.RawMessage `json:"message" doc:"The transcript message as JSON"`
}

type EventsOutput struct {
	Body []EventBody
}

func sessionToBody(s gendb.Session) SessionBody {
	return SessionBody{
		ID:        s.ID,
		Prompt:    s.Prompt,
		Workdir:   s.Workdir,
		Status:    s.Status,
		FinalText: s.FinalText,
		Error:     s.Error,
		CreatedAt: s.CreatedAt,
	}
}

// sessionHandlers holds the dependencies the session endpoints need. Its methods
// are the handlers; they are wired to routes in serve.go.
type sessionHandlers struct {
	queries *gendb.Queries
	agent   Agent
}

// Create runs the agent synchronously for this slice: the request blocks until
// the agent stops, then returns the result.
func (h *sessionHandlers) Create(ctx context.Context, input *CreateSessionInput) (*SessionOutput, error) {
	// Each session gets a fresh local-machine workspace. Created up front so
	// it lands in the persisted session row even if the run fails.
	workdir, err := newWorkspace()
	if err != nil {
		return nil, huma.Error500InternalServerError("creating workspace", err)
	}

	session, err := h.queries.CreateSession(ctx, gendb.CreateSessionParams{
		Prompt:  input.Body.Prompt,
		Workdir: workdir,
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("creating session", err)
	}

	machine := &LocalMachine{Workdir: workdir}
	result, runErr := h.agent.Run(ctx, machine, input.Body.Prompt)

	// Persist whatever transcript we got, even on failure.
	h.persistTranscript(ctx, session.ID, result.Transcript)

	status, errText, finalText := "done", "", result.FinalText
	if runErr != nil {
		slog.Error("failed to run agent session", "session", session.ID, "err", runErr)
		status, errText, finalText = "error", runErr.Error(), ""
	}
	finished, err := h.queries.FinishSession(ctx, gendb.FinishSessionParams{
		Status:    status,
		FinalText: finalText,
		Error:     errText,
		ID:        session.ID,
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("finishing session", err)
	}
	return &SessionOutput{Body: sessionToBody(finished)}, nil
}

func (h *sessionHandlers) Get(ctx context.Context, input *GetSessionInput) (*SessionOutput, error) {
	session, err := h.queries.GetSession(ctx, input.ID)
	if err != nil {
		return nil, huma.Error404NotFound("session not found")
	}
	return &SessionOutput{Body: sessionToBody(session)}, nil
}

func (h *sessionHandlers) ListEvents(ctx context.Context, input *GetSessionInput) (*EventsOutput, error) {
	events, err := h.queries.ListSessionEvents(ctx, input.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("listing events", err)
	}
	out := &EventsOutput{Body: make([]EventBody, 0, len(events))}
	for _, e := range events {
		out.Body = append(out.Body, EventBody{
			Seq:     e.Seq,
			Role:    e.Role,
			Message: json.RawMessage(e.Message),
		})
	}
	return out, nil
}

// persistTranscript writes each message of the transcript as a session event.
// Best-effort: a write failure is swallowed rather than failing the whole
// request, since the run itself already happened.
func (h *sessionHandlers) persistTranscript(ctx context.Context, sessionID int64, transcript []TranscriptMessage) {
	for i, msg := range transcript {
		if _, err := h.queries.AppendSessionEvent(ctx, gendb.AppendSessionEventParams{
			SessionID: sessionID,
			Seq:       int64(i),
			Role:      msg.Role,
			Message:   string(msg.JSON),
		}); err != nil {
			slog.Error("failed to persist session event", "session", sessionID, "seq", i, "err", err)
		}
	}
}

// newWorkspace creates a fresh directory for a local machine to run in.
func newWorkspace() (string, error) {
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(workspaceRoot, "session-")
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolving workspace path: %w", err)
	}
	return abs, nil
}
