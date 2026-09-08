// Package learningspace defines the shared business scope contract. Legacy
// resources belong to DefaultID until their owning modules migrate explicitly.
package learningspace

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const DefaultID = "00000000-0000-4000-8000-000000000001"
const Header = "X-Learning-Space-ID"

type Space struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	Version     int64     `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
type Command struct {
	OperationID     string `json:"operation_id"`
	ExpectedVersion int64  `json:"expected_version"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	Status          string `json:"status"`
}
type Query struct {
	Search, Status, Cursor string
	Limit                  int
}
type Page struct {
	Items      []Space `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}
type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }
func Invalid() error           { return &Error{Code: "invalid_learning_space"} }
func ValidID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u != uuid.Nil && u.String() == id
}
func (c Command) Validate(create bool) error {
	if !ValidID(c.OperationID) || (create && c.ExpectedVersion != 0) || (!create && c.ExpectedVersion < 1) ||
		strings.TrimSpace(c.Name) == "" || utf8.RuneCountInString(c.Name) > 120 || !utf8.ValidString(c.Name) ||
		utf8.RuneCountInString(c.Description) > 2000 || !utf8.ValidString(c.Description) ||
		(c.Status != "active" && c.Status != "archived") || (create && c.Status != "active") {
		return Invalid()
	}
	return nil
}

type scopeKey struct{}

func WithScope(ctx context.Context, id string) (context.Context, error) {
	if !ValidID(id) {
		return nil, Invalid()
	}
	return context.WithValue(ctx, scopeKey{}, id), nil
}
func Scope(ctx context.Context) string {
	if id, ok := ctx.Value(scopeKey{}).(string); ok {
		return id
	}
	return DefaultID
}

// RequireLegacyModule prevents callers from presenting global data as isolated.
func RequireLegacyModule(ctx context.Context) error {
	if Scope(ctx) != DefaultID {
		return &Error{Code: "learning_space_module_unavailable"}
	}
	return nil
}
