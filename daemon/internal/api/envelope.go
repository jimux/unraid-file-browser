package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"

	"unraid-filebrowser/internal/types"
)

// envelope is the JSON shape of every non-raw response (API.md).
type envelope struct {
	OK    bool       `json:"ok"`
	Data  any        `json:"data,omitempty"`
	Error *errorBody `json:"error,omitempty"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// statusFor maps an API error code onto its HTTP status.
func statusFor(code string) int {
	switch code {
	case types.ErrBadRequest:
		return http.StatusBadRequest
	case types.ErrNotFound:
		return http.StatusNotFound
	case types.ErrForbidden:
		return http.StatusForbidden
	case types.ErrTooLarge:
		return http.StatusRequestEntityTooLarge
	case types.ErrTimeout:
		return http.StatusGatewayTimeout
	case types.ErrArchive, types.ErrEncoding:
		return http.StatusUnprocessableEntity
	case types.ErrIndexing:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(envelope{OK: true, Data: data})
}

// writeError renders err as the error envelope, mapping unrecognised errors to
// INTERNAL (and logging them — an unmapped error is a daemon bug).
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := classify(r, err)
	if code == types.ErrInternal {
		slog.Error("api: internal error", "method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery, "err", err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusFor(code))
	_ = json.NewEncoder(w).Encode(envelope{OK: false, Error: &errorBody{Code: code, Message: msg}})
}

// classify turns any error into an API code plus a message safe to show.
func classify(r *http.Request, err error) (string, string) {
	var apiErr *types.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code, apiErr.Message
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return types.ErrTimeout, "the request took too long"
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		// Client hung up; the response goes nowhere but keep the code honest.
		return types.ErrTimeout, "request cancelled"
	case errors.Is(err, fs.ErrNotExist):
		return types.ErrNotFound, "no such file or directory"
	case errors.Is(err, fs.ErrPermission):
		return types.ErrForbidden, "permission denied"
	default:
		return types.ErrInternal, "internal error"
	}
}
