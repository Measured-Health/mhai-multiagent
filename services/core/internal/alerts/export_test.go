package alerts

import "net/http"

// WithStepUpCheck returns d with its step-up check replaced (tests only).
func WithStepUpCheck(d Deps, f func(r *http.Request, mfaCode, backupCode string) error) Deps {
	d.stepUpCheck = f
	return d
}

// Handlers maps "METHOD pattern" to each route's handler, for tests that
// serve them with a principal of their own in the context.
func Handlers(d Deps) map[string]http.Handler {
	return map[string]http.Handler{
		"GET /hospital/alert-settings":                http.HandlerFunc(d.settingsGet),
		"PATCH /hospital/alert-settings":              http.HandlerFunc(d.settingsPatch),
		"GET /hospital/patient-matches":               http.HandlerFunc(d.matchesList),
		"POST /hospital/patient-matches/{id}/confirm": http.HandlerFunc(d.matchConfirm),
		"POST /hospital/patient-matches/{id}/reject":  http.HandlerFunc(d.matchReject),
		"POST /hospital/patient-exclusions":           http.HandlerFunc(d.exclude),
		"GET /alerts":                                 http.HandlerFunc(d.list),
		"GET /alerts/unread-count":                    http.HandlerFunc(d.unreadCount),
		"POST /alerts/{id}/acknowledge":               d.handle("acknowledge"),
		"POST /alerts/{id}/dismiss":                   d.handle("dismiss"),
		"GET /me/notification-settings":               http.HandlerFunc(d.notifyGet),
		"PATCH /me/notification-settings":             http.HandlerFunc(d.notifyPatch),
	}
}
