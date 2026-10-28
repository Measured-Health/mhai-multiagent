// Package httpx is the gateway's HTTP foundation: the /api/v1 router, the
// error catalogue and problem bodies, request ids, the BFF service
// credential and Idempotency-Key handling.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
)

// Codes maps every error code the gateway returns to its HTTP status
// (Global Constraints). Nothing else may be sent as a code.
var Codes = map[string]int{
	"unauthenticated":   http.StatusUnauthorized,
	"token_expired":     http.StatusUnauthorized,
	"mfa_required":      http.StatusForbidden,
	"mfa_invalid":       http.StatusForbidden,
	"mfa_locked":        http.StatusTooManyRequests,
	"forbidden":         http.StatusForbidden,
	"not_platform":      http.StatusForbidden,
	"not_found":         http.StatusNotFound,
	"conflict":          http.StatusConflict,
	"validation_failed": http.StatusUnprocessableEntity,
	"weak_password":     http.StatusUnprocessableEntity,
	"rate_limited":      http.StatusTooManyRequests,
	"login_locked":      http.StatusTooManyRequests,
	"not_ready":         http.StatusServiceUnavailable,
	"unavailable":       http.StatusServiceUnavailable, // intelligence didn't answer (spec §10.1)
	"internal_error":    http.StatusInternalServerError,
}

// titles and details are fixed per code. They never carry input or error
// text, so a problem body can't leak what the client sent or why a query
// failed.
var titles = map[string]string{
	"unauthenticated":   "Unauthenticated",
	"token_expired":     "Token expired",
	"mfa_required":      "Two-step verification required",
	"mfa_invalid":       "Invalid verification code",
	"mfa_locked":        "Too many verification attempts",
	"forbidden":         "Forbidden",
	"not_platform":      "Platform accounts only",
	"not_found":         "Not found",
	"conflict":          "Conflict",
	"validation_failed": "Validation failed",
	"weak_password":     "Weak password",
	"rate_limited":      "Too many requests",
	"login_locked":      "Sign-in locked",
	"not_ready":         "Not ready",
	"unavailable":       "Unavailable",
	"internal_error":    "Internal error",
}

var details = map[string]string{
	"unauthenticated":   "Sign-in is required.",
	"token_expired":     "The access token has expired.",
	"mfa_required":      "Two-step verification is required.",
	"mfa_invalid":       "The verification code is not valid.",
	"mfa_locked":        "Too many verification attempts. Try again later.",
	"forbidden":         "You do not have permission to do this.",
	"not_platform":      "This route is for platform accounts only.",
	"not_found":         "The resource was not found.",
	"conflict":          "The request conflicts with the current state.",
	"validation_failed": "The request is not valid.",
	"weak_password":     "The password does not meet the requirements.",
	"rate_limited":      "Too many requests. Try again later.",
	"login_locked":      "Too many sign-in attempts. Try again later.",
	"not_ready":         "The service is not ready.",
	"unavailable":       "Couldn't get an answer. Try again.",
	"internal_error":    "Something went wrong.",
}

// problemTypeBase prefixes the problem "type" URI.
const problemTypeBase = "urn:mhai:problem:"

type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Code      string `json:"code"`
	Detail    string `json:"detail"`
	RequestID string `json:"request_id"`
}

// Error writes the error for code: an application/problem+json body, or
// exactly {"code": code} on the platform audience. An unknown code is sent
// as internal_error.
func Error(w http.ResponseWriter, r *http.Request, code string) {
	status, ok := Codes[code]
	if !ok {
		code, status = "internal_error", http.StatusInternalServerError
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	var body []byte
	if Audience(r.Context()) == AudiencePlatform {
		h.Set("Content-Type", "application/json")
		body, _ = json.Marshal(struct {
			Code string `json:"code"`
		}{code})
	} else {
		h.Set("Content-Type", "application/problem+json")
		body, _ = json.Marshal(problem{
			Type:      problemTypeBase + code,
			Title:     titles[code],
			Status:    status,
			Code:      code,
			Detail:    details[code],
			RequestID: RequestIDFrom(r.Context()),
		})
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ErrorRetryAfter is Error with a Retry-After header of seconds (whole
// seconds, only when positive), for a refusal the client may retry later.
func ErrorRetryAfter(w http.ResponseWriter, r *http.Request, code string, seconds int) {
	if seconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
	Error(w, r, code)
}

// JSON writes v as an application/json response.
func JSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"code":"internal_error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Decode reads one JSON object from the request body into v, rejecting
// unknown fields, trailing data and bodies over MaxBody. On failure it
// writes 422 validation_failed (never echoing the input) and returns false.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decodeStrict(r.Body, v); err != nil {
		Error(w, r, "validation_failed")
		return false
	}
	return true
}

func decodeStrict(body io.Reader, v any) error {
	if body == nil {
		return errors.New("no body")
	}
	dec := json.NewDecoder(io.LimitReader(body, MaxBody+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}
