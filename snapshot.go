// Snapshot fetcher. Dahua VTOs expose
//
//	/cgi-bin/snapshot.cgi?channel=1
//
// returning an `image/jpeg` body authenticated with the same HTTP-Digest
// credentials we already use for door open. The flow is therefore a clone
// of door.go's Open(): first GET → 401 → parse challenge → second GET with
// the computed Authorization → JPEG bytes.
//
// We deliberately keep the function in its own file (rather than reusing
// DoorClient.send) because the response handling differs — door open just
// cares about a 2xx, snapshot must drain the JPEG body into memory.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// maxSnapshotBytes caps the JPEG we'll buffer from the camera. Dahua VTO
// snapshots top out around 100-150 KB at default resolution; 2 MB is
// generous headroom while still bounding a misbehaving / spoofed peer.
const maxSnapshotBytes = 2 * 1024 * 1024

func snapshotURLFor(s StreamConfig) string {
	return fmt.Sprintf("http://%s/cgi-bin/snapshot.cgi?channel=1", s.Host)
}

// FetchSnapshot performs the digest-authed GET and returns the JPEG body.
// httpClient is reused so we can share connection pooling with other
// per-camera HTTP traffic; pass a client with a sensible Timeout.
func FetchSnapshot(ctx context.Context, httpClient *http.Client, s StreamConfig) ([]byte, error) {
	target := snapshotURLFor(s)

	resp, err := snapshotSend(ctx, httpClient, target, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return readSnapshotBody(resp)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		drainAndClose(resp.Body)
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	chal, err := parseDigestChallenge(resp.Header.Get("WWW-Authenticate"))
	drainAndClose(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse challenge: %w", err)
	}

	parsed, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("parse snapshot url: %w", err)
	}
	auth := buildDigestResponse(http.MethodGet, parsed.RequestURI(), s.User, s.Pass, chal)

	authed, err := snapshotSend(ctx, httpClient, target, auth)
	if err != nil {
		return nil, err
	}
	if authed.StatusCode != http.StatusOK {
		drainAndClose(authed.Body)
		return nil, fmt.Errorf("auth failed: status %d", authed.StatusCode)
	}
	return readSnapshotBody(authed)
}

func snapshotSend(ctx context.Context, c *http.Client, target, authHeader string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return c.Do(req)
}

func readSnapshotBody(resp *http.Response) ([]byte, error) {
	defer drainAndClose(resp.Body)
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshotBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > maxSnapshotBytes {
		return nil, fmt.Errorf("snapshot exceeds %d bytes", maxSnapshotBytes)
	}
	return body, nil
}

// newSnapshotClient returns the http.Client snapshot fetches should use.
// Snapshots are small but the camera occasionally takes a beat to encode
// them when it's also serving RTSP, so the timeout is a bit looser than
// door open.
func newSnapshotClient() *http.Client {
	return &http.Client{Timeout: snapshotTimeout + 2*time.Second}
}
