package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"charm.land/fantasy"
)

var ErrNotFound = errors.New("session not found")

type SessionStore interface {
	Save(ctx context.Context, s *Session) error
	Load(ctx context.Context, id string) (*Session, error)
}

// JSONLStore writes <dir>/<id>.jsonl: line 1 = meta, each later line = one message.
type JSONLStore struct{ dir string }

func NewJSONLStore(dir string) (*JSONLStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &JSONLStore{dir: dir}, nil
}

func (s *JSONLStore) path(id string) string { return filepath.Join(s.dir, id+".jsonl") }

type sessionMeta struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Status  Status        `json:"status"`
	Usage   fantasy.Usage `json:"usage"`
	Sandbox SandboxSpec   `json:"sandbox"`
}

type record struct {
	Kind    string           `json:"kind"` // "meta" | "message"
	Meta    *sessionMeta     `json:"meta,omitempty"`
	Message *fantasy.Message `json:"message,omitempty"`
}

func (s *JSONLStore) Save(_ context.Context, sess *Session) error {
	tmp, err := os.CreateTemp(s.dir, sess.ID+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	w := bufio.NewWriter(tmp)
	enc := json.NewEncoder(w)
	writeErr := enc.Encode(record{Kind: "meta", Meta: &sessionMeta{
		ID: sess.ID, Model: sess.Model, Status: sess.Status,
		Usage: sess.Usage, Sandbox: sess.Sandbox,
	}})
	for i := range sess.Messages {
		if writeErr != nil {
			break
		}
		writeErr = enc.Encode(record{Kind: "message", Message: &sess.Messages[i]})
	}
	if writeErr == nil {
		writeErr = w.Flush()
	}
	if writeErr != nil {
		tmp.Close()
		return writeErr
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path(sess.ID)) // atomic
}

func (s *JSONLStore) Load(_ context.Context, id string) (*Session, error) {
	f, err := os.Open(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sess := &Session{ID: id}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // tool output can be big

	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, err
		}
		switch rec.Kind {
		case "meta":
			if rec.Meta != nil {
				sess.Model, sess.Status = rec.Meta.Model, rec.Meta.Status
				sess.Usage, sess.Sandbox = rec.Meta.Usage, rec.Meta.Sandbox
			}
		case "message":
			if rec.Message != nil {
				sess.Messages = append(sess.Messages, *rec.Message)
			}
		}
	}
	return sess, sc.Err()
}
