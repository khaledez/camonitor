package main

import "net/http"

// buildSurfaces keeps admin routes off the LAN listener: only the
// tailnet surface mounts them, so :8080 never exposes guest management.
func buildSurfaces(base http.Handler, admin http.Handler) (lan http.Handler, ts http.Handler) {
	tsMux := http.NewServeMux()
	tsMux.Handle("/admin/", admin)
	tsMux.Handle("/", base)
	return base, tsMux
}
