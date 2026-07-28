package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
)

var ErrNotFound = errors.New("session not found")

type SessionStore interface {
	Save(ctx context.Context, s *Session) error
	Load(ctx context.Context, id string) (*Session, error)
}

// SessionLister is an optional store capability for discovering resumable
// sessions.
type SessionLister interface {
	List(ctx context.Context, limit int) ([]SessionInfo, error)
}

// SessionInfo is the summary needed to choose a session without loading its
// transcript.
type SessionInfo struct {
	ID        string
	Model     string
	Status    Status
	Messages  int
	Usage     fantasy.Usage
	UpdatedAt time.Time
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
	Sandbox sandboxName   `json:"sandbox"`
}

// sandboxName is the sandbox a session recorded. Only the name is ever written:
// an image, a backend, a daemon address are this process's configuration, and
// the process that resumes the session is free to differ on all of them — the
// name is the only part that still means the same thing.
type sandboxName string

// UnmarshalJSON also reads the object older sessions recorded, {"name":…,
// "image":…}, keeping the name and dropping the rest.
func (n *sandboxName) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err == nil {
		*n = sandboxName(name)
		return nil
	}
	var legacy struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &legacy); err != nil {
		return err
	}
	*n = sandboxName(legacy.Name)
	return nil
}

type record struct {
	Kind    string           `json:"kind"` // "meta" | "message"
	Meta    *sessionMeta     `json:"meta,omitempty"`
	Message *fantasy.Message `json:"message,omitempty"`
}

func (s *JSONLStore) Save(ctx context.Context, sess *Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
		Usage: sess.Usage, Sandbox: sandboxName(sess.name),
	}})
	for i := range sess.Messages {
		if writeErr != nil {
			break
		}
		if writeErr = ctx.Err(); writeErr != nil {
			break
		}
		writeErr = enc.Encode(record{Kind: "message", Message: &sess.Messages[i]})
	}
	if writeErr == nil {
		writeErr = ctx.Err()
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
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path(sess.ID)) // atomic
}

func (s *JSONLStore) Load(ctx context.Context, id string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
				sess.Usage = rec.Meta.Usage
				// The name only: binding it to a live sandbox is the caller's,
				// which is what keeps a store out of the sandbox business.
				sess.name = string(rec.Meta.Sandbox)
			}
		case "message":
			if rec.Message != nil {
				sess.Messages = append(sess.Messages, *rec.Message)
			}
		}
	}
	return sess, sc.Err()
}

// List returns the most recently saved sessions first.
func (s *JSONLStore) List(ctx context.Context, limit int) ([]SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []SessionInfo{}, nil
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	infos := make([]SessionInfo, 0, min(limit, len(entries)))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		sess, err := s.Load(ctx, id)
		if err != nil {
			return nil, err
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return nil, err
		}
		infos = append(infos, SessionInfo{
			ID:        sess.ID,
			Model:     sess.Model,
			Status:    sess.Status,
			Messages:  len(sess.Messages),
			Usage:     sess.Usage,
			UpdatedAt: fileInfo.ModTime(),
		})
	}
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].UpdatedAt.Equal(infos[j].UpdatedAt) {
			return infos[i].ID < infos[j].ID
		}
		return infos[i].UpdatedAt.After(infos[j].UpdatedAt)
	})
	if len(infos) > limit {
		infos = infos[:limit]
	}
	return infos, nil
}
