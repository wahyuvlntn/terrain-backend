package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"
)

// Error is an error carrying an HTTP status code and a user facing message.
//
// The web UI translates messages: a plain key such as "err_asset_not_found"
// is looked up in its locale files, and "PREFIX:key:param1:param2" fills
// {{$n}} placeholders. Messages that are not keys are displayed verbatim, so
// they must not contain ':'.
type Error struct {
	Status int
	Msg    string
	Err    error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// Key formats a translatable message with parameters.
func Key(key string, params ...any) string {
	if len(params) == 0 {
		return key
	}
	parts := []string{"MSG", key}
	for _, p := range params {
		parts = append(parts, sanitizeParam(fmt.Sprint(p)))
	}
	return strings.Join(parts, ":")
}

func sanitizeParam(s string) string {
	s = strings.ReplaceAll(s, ":", " ")
	return strings.TrimSpace(s)
}

// BadRequest returns a 400 error.
func BadRequest(msg string) *Error { return &Error{Status: http.StatusBadRequest, Msg: msg} }

// BadRequestf returns a 400 error with a formatted message.
func BadRequestf(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Msg: fmt.Sprintf(format, args...)}
}

// NotFound returns a 404 error.
func NotFound(msg string) *Error { return &Error{Status: http.StatusNotFound, Msg: msg} }

// Forbidden returns a 403 error.
func Forbidden(msg string) *Error {
	if msg == "" {
		msg = "You do not have permission to perform this action"
	}
	return &Error{Status: http.StatusForbidden, Msg: msg}
}

// Unauthorized returns a 401 error (the UI redirects to the sign-in page).
func Unauthorized() *Error {
	return &Error{Status: http.StatusUnauthorized, Msg: "Unauthorized, please re-login"}
}

// Conflict returns a 409 error.
func Conflict(msg string) *Error { return &Error{Status: http.StatusConflict, Msg: msg} }

// Internal wraps an unexpected error.
func Internal(err error) *Error {
	return &Error{Status: http.StatusInternalServerError, Msg: "err_something_went_wrong", Err: err}
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		slog.Debug("write json", "err", err)
	}
}

// WriteEnvelope writes the js-service style envelope {data, error}.
func WriteEnvelope(w http.ResponseWriter, data any) {
	WriteJSON(w, http.StatusOK, map[string]any{"data": data, "error": nil})
}

// WriteError writes an error response understood by the web UI.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var he *Error
	if !errors.As(err, &he) {
		he = Internal(err)
	}
	if he.Status >= 500 {
		slog.Error("request failed", "path", r.URL.Path, "err", err)
	} else {
		slog.Debug("request rejected", "path", r.URL.Path, "status", he.Status, "msg", he.Msg)
	}
	WriteJSON(w, he.Status, map[string]any{
		"data":    nil,
		"error":   he.Msg,
		"message": he.Msg,
	})
}

// TS formats a time the way JavaScript's Date#toISOString does, which is the
// only format accepted by the UI's z.string().datetime() validators.
func TS(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// TSPtr formats an optional time (nil -> JSON null).
func TSPtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return TS(*t)
}

// Round rounds f to n decimals, mapping NaN/Inf to 0.
func Round(f float64, n int) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	p := math.Pow(10, float64(n))
	return math.Round(f*p) / p
}
