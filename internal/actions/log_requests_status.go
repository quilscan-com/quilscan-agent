package actions

import "time"

// LogRequestsInvalidationPatch clears volatile owner queue/preview data on restart.
func LogRequestsInvalidationPatch(reason string) map[string]interface{} {
	now := time.Now().UTC().Format(time.RFC3339)
	unavailable := func() map[string]interface{} {
		return map[string]interface{}{"command_id": "", "read_state": "unavailable", "error": reason, "refreshed_at": now, "data": nil}
	}
	return map[string]interface{}{"qclient_log_requests": unavailable(), "qclient_log_preview": unavailable()}
}
