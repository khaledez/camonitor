package main

import (
	"log"
	"net/http"
	"time"
)

// buildSurfaces keeps admin routes off the LAN listener: only the
// tailnet surface mounts them, so :8080 never exposes guest management.
func buildSurfaces(base http.Handler, admin http.Handler) (lan http.Handler, ts http.Handler) {
	tsMux := http.NewServeMux()
	tsMux.Handle("/admin/", admin)
	tsMux.Handle("/", base)
	return base, tsMux
}

// pruneAuditDaily drops old rows; OpenAuditStore already pruned once at startup.
func pruneAuditDaily(audit *AuditStore) {
	if audit == nil {
		return
	}
	for range time.Tick(24 * time.Hour) {
		if _, err := audit.Prune(time.Now().Add(-auditRetention)); err != nil {
			log.Printf("guest: audit prune failed: %v", err)
		}
	}
}
