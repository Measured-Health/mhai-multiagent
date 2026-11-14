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
	"unavailable":       http.StatusServiceUnavailable, // intelligence didn't answer (spec §10.1), or the NATS API is full
	"internal_error":    http.StatusInternalServerError,
	// Over NATS only (internal/natsapi): a Mhai-Method outside the six the
	// API uses, and a body over MaxBody, refused before the handler chain.
	"method_not_allowed": http.StatusMethodNotAllowed,
	"payload_too_large":  http.StatusRequestEntityTooLarge,
	// Plan 4 (ADR 0012, additive): the two-person rule on corrections.
	"self_approval":   http.StatusForbidden,
	"already_decided": http.StatusConflict,
	// EMR integration (design §16, additive): switching on without a
	// passing test of the saved settings; a connection write for a removed
	// or removal-pending hospital; a connection type not offered.
	"not_tested":       http.StatusConflict,
	"hospital_removed": http.StatusConflict,
	"no_type":          http.StatusUnprocessableEntity,
	// Patient Analysis (analysis backend design §9, additive): more than
	// two filters; a period that isn't exactly one answerable frozen period.
	"too_many_filters": http.StatusUnprocessableEntity,
	"invalid_range":    http.StatusUnprocessableEntity,
	// The de-identified drill-down (design §9): a drill that isn't a shown
	// count of 5 or more of this context; a level other than deidentified.
	"too_small":           http.StatusUnprocessableEntity,
	"level_not_supported": http.StatusUnprocessableEntity,
	// EMR alerts (design §16, additive): an alert someone handled first
	// (the body names who and when); alert settings with no recipient.
	"already_handled": http.StatusConflict,
	"no_recipients":   http.StatusUnprocessableEntity,
	// The hospital admin's staff routes (frontend requirements §5.1;
	// Analysis Task 17, additive): an admin's change to their own account
	// that isn't allowed; no active hospital admin would be left; the
	// Analyst grant's refusals (migration 00030); and a route the app
	// calls that core has no feature behind yet.
	"self_change":     http.StatusForbidden,
	"last_admin":      http.StatusConflict,
	"analysis_off":    http.StatusConflict,
	"analyst_cap":     http.StatusConflict,
	"not_eligible":    http.StatusUnprocessableEntity,
	"not_implemented": http.StatusNotImplemented,
	// The platform admin routes (additive): the team's own-role rule, and
	// the two-person rule on a hold's lift and a lapsed request.
	"own_role":    http.StatusConflict,
	"same_person": http.StatusConflict,
	"expired":     http.StatusConflict,
	// Clearance letters, hospital side (analysis backend design §8.1, §9;
	// additive): named access off for the hospital; a letter body past the
	// limit, or not multipart; an end date past the 12-month cap; platform
	// admins a letter can't cover; narrowing to nobody.
	"named_access_off":      http.StatusForbidden,
	"file_too_large":        http.StatusRequestEntityTooLarge,
	"file_type":             http.StatusUnsupportedMediaType,
	"end_date_too_late":     http.StatusUnprocessableEntity,
	"not_eligible_platform": http.StatusUnprocessableEntity,
	"would_empty":           http.StatusUnprocessableEntity,
	// Named access (analysis backend design §8.3, §8.5; additive): no
	// letter covers the person, or the one that did has just ended; an
	// Idempotency-Key used by another person or with another body.
	"not_cleared":          http.StatusForbidden,
	"clearance_ended":      http.StatusForbidden,
	"idempotency_mismatch": http.StatusUnprocessableEntity,
}

// titles and details are fixed per code. They never carry input or error
// text, so a problem body can't leak what the client sent or why a query
// failed.
var titles = map[string]string{
	"unauthenticated":       "Unauthenticated",
	"token_expired":         "Token expired",
	"mfa_required":          "Two-step verification required",
	"mfa_invalid":           "Invalid verification code",
	"mfa_locked":            "Too many verification attempts",
	"forbidden":             "Forbidden",
	"not_platform":          "Platform accounts only",
	"not_found":             "Not found",
	"conflict":              "Conflict",
	"validation_failed":     "Validation failed",
	"weak_password":         "Weak password",
	"rate_limited":          "Too many requests",
	"login_locked":          "Sign-in locked",
	"not_ready":             "Not ready",
	"unavailable":           "Unavailable",
	"internal_error":        "Internal error",
	"method_not_allowed":    "Method not allowed",
	"payload_too_large":     "Payload too large",
	"self_approval":         "Not allowed",
	"already_decided":       "Already decided",
	"not_tested":            "Not tested",
	"hospital_removed":      "Hospital removed",
	"no_type":               "Unknown connection type",
	"too_many_filters":      "Too many filters",
	"invalid_range":         "Invalid period",
	"too_small":             "Group too small",
	"level_not_supported":   "Level not supported",
	"already_handled":       "Already handled",
	"no_recipients":         "No recipients",
	"self_change":           "Not allowed on your own account",
	"last_admin":            "Last hospital admin",
	"analysis_off":          "Analysis is off",
	"analyst_cap":           "Analyst limit reached",
	"not_eligible":          "Not eligible",
	"not_implemented":       "Not available",
	"own_role":              "Own role",
	"same_person":           "Same person",
	"expired":               "Expired",
	"named_access_off":      "Named access is off",
	"file_too_large":        "File too large",
	"file_type":             "File type not accepted",
	"end_date_too_late":     "End date too late",
	"not_eligible_platform": "Not eligible",
	"would_empty":           "Would cover nobody",
	"not_cleared":           "Not cleared",
	"clearance_ended":       "Clearance ended",
	"idempotency_mismatch":  "Idempotency key mismatch",
}

var details = map[string]string{
	"unauthenticated":       "Sign-in is required.",
	"token_expired":         "The access token has expired.",
	"mfa_required":          "Two-step verification is required.",
	"mfa_invalid":           "The verification code is not valid.",
	"mfa_locked":            "Too many verification attempts. Try again later.",
	"forbidden":             "You do not have permission to do this.",
	"not_platform":          "This route is for platform accounts only.",
	"not_found":             "The resource was not found.",
	"conflict":              "The request conflicts with the current state.",
	"validation_failed":     "The request is not valid.",
	"weak_password":         "The password does not meet the requirements.",
	"rate_limited":          "Too many requests. Try again later.",
	"login_locked":          "Too many sign-in attempts. Try again later.",
	"not_ready":             "The service is not ready.",
	"unavailable":           "Couldn't get an answer. Try again.",
	"internal_error":        "Something went wrong.",
	"method_not_allowed":    "The method is not allowed.",
	"payload_too_large":     "The request body is too large.",
	"self_approval":         "You can't decide your own correction.",
	"already_decided":       "This correction has already been decided.",
	"not_tested":            "Test the saved settings before switching the connection on.",
	"hospital_removed":      "The hospital is removed or being removed.",
	"no_type":               "This connection type is not offered.",
	"too_many_filters":      "At most two filters can be used together.",
	"invalid_range":         "Choose one finished period.",
	"too_small":             "This group can't be listed.",
	"level_not_supported":   "Only the de-identified list is available.",
	"already_handled":       "Someone has already handled this alert.",
	"no_recipients":         "Choose the queue or at least one clinician.",
	"self_change":           "You can't make this change to your own account.",
	"last_admin":            "The hospital needs at least one active hospital admin.",
	"analysis_off":          "Patient Analysis is off for this hospital.",
	"analyst_cap":           "The hospital already has the most analysts allowed.",
	"not_eligible":          "This person isn't eligible for this.",
	"not_implemented":       "This isn't available yet.",
	"own_role":              "You can't change your own role.",
	"same_person":           "A different person must approve this.",
	"expired":               "This request has lapsed.",
	"named_access_off":      "Named access isn't available for this hospital.",
	"file_too_large":        "The letter is too large.",
	"file_type":             "Send the letter as a multipart upload.",
	"end_date_too_late":     "The end date must be after today and at most 12 months away.",
	"not_eligible_platform": "Some Measured Health staff can't be covered by a letter.",
	"would_empty":           "A letter must cover someone; revoke it instead.",
	"not_cleared":           "No current clearance letter covers you for named access.",
	"clearance_ended":       "The clearance letter that covered you has ended.",
	"idempotency_mismatch":  "This Idempotency-Key was used for another request.",
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

// ErrorFields is Error on the clinical audience with extra members in the
// body, for a refusal whose reply the frontend contract shapes: the 409
// already_handled names the alert's status (status, as mhai-web's
// AlertHandledOut reads it, in place of the problem's number), who handled
// it and when (design §12.5). extra may replace title, detail and status;
// never type, code or request_id. Its values are the server's own, never
// input. On the platform audience it is Error.
func ErrorFields(w http.ResponseWriter, r *http.Request, code string, extra map[string]any) {
	status, ok := Codes[code]
	if !ok || Audience(r.Context()) == AudiencePlatform {
		Error(w, r, code)
		return
	}
	body := map[string]any{"title": titles[code], "status": status, "detail": details[code]}
	for k, v := range extra {
		body[k] = v
	}
	body["type"], body["code"], body["request_id"] = problemTypeBase+code, code, RequestIDFrom(r.Context())
	raw, err := json.Marshal(body)
	if err != nil {
		Error(w, r, "internal_error")
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
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
