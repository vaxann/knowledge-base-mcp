// Package kb is the service layer: it combines the vault, the Git repository,
// the catalog and the search index behind a single write lock, and implements
// every operation the MCP tools expose.
package kb

import (
	"errors"
	"fmt"

	"github.com/vaxann/knowledge-base-mcp/internal/gitrepo"
	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// Error is a tool-visible failure with a stable code.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// E builds an Error.
func E(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// With attaches details.
func (e *Error) With(k string, v any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[k] = v
	return e
}

// Stable error codes.
const (
	CodeInvalidPath     = "invalid_path"
	CodeNotFound        = "not_found"
	CodeAlreadyExists   = "already_exists"
	CodeConflict        = "conflict"
	CodeMergeConflict   = "merge_conflict"
	CodeMarkersPresent  = "conflict_markers_present"
	CodePatchFailed     = "patch_failed"
	CodeUnsupportedFile = "unsupported_file"
	CodeReadOnly        = "read_only"
	CodeInvalidArgument = "invalid_argument"
	CodeInternal        = "internal"
	CodeNoRemote        = "no_remote"
	CodeNotInConflict   = "not_in_conflict"
	CodeTooLarge        = "too_large"
)

// wrap converts lower-level errors into tool errors.
func wrap(err error) *Error {
	if err == nil {
		return nil
	}
	var ke *Error
	if errors.As(err, &ke) {
		return ke
	}
	switch {
	case errors.Is(err, vault.ErrInvalidPath):
		return E(CodeInvalidPath, "%s", err.Error())
	case errors.Is(err, vault.ErrNotFound), errors.Is(err, gitrepo.ErrNotFound):
		return E(CodeNotFound, "%s", err.Error())
	case errors.Is(err, vault.ErrSectionNotFound):
		return E(CodeNotFound, "%s", err.Error())
	}
	return E(CodeInternal, "%s", err.Error())
}
