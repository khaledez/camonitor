// HTTP Digest Access Authentication helpers (RFC 2617), shared between the
// RTSP client (rtsp.go) and the door-control HTTP client (door.go). RTSP
// reuses the same digest scheme as HTTP — only the request line differs.
//
// We support two challenge variants the Dahua firmware emits:
//
//   - Legacy "no-qop" form, used by Dahua's RTSP server.
//     response = MD5(HA1:nonce:HA2)
//
//   - RFC 2617 qop="auth" form, used by Dahua's HTTP server. Requires us to
//     send back cnonce + nc and recompute the response.
//     response = MD5(HA1:nonce:nc:cnonce:qop:HA2)
//
// A camera that gets the wrong form replies with "401 Invalid Authority!",
// which sounds like a permissions problem but is actually an auth-response
// mismatch.
package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

type digestChallenge struct {
	realm, nonce, qop, opaque string
}

func parseDigestChallenge(header string) (*digestChallenge, error) {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(strings.ToLower(header), "digest ") {
		return nil, fmt.Errorf("not a digest challenge: %q", header)
	}
	params := splitDigestParams(header[len("digest "):])
	c := &digestChallenge{
		realm:  params["realm"],
		nonce:  params["nonce"],
		qop:    params["qop"],
		opaque: params["opaque"],
	}
	if c.realm == "" || c.nonce == "" {
		return nil, errors.New("digest challenge missing realm or nonce")
	}
	return c, nil
}

// splitDigestParams parses a comma-separated list of key=value pairs where
// values may be quoted strings containing commas.
func splitDigestParams(s string) map[string]string {
	out := map[string]string{}
	inQuotes := false
	start := 0
	flush := func(end int) {
		part := strings.TrimSpace(s[start:end])
		if part == "" {
			return
		}
		before, after, ok := strings.Cut(part, "=")
		if !ok {
			return
		}
		key := strings.ToLower(strings.TrimSpace(before))
		val := strings.TrimSpace(after)
		val = strings.TrimPrefix(val, `"`)
		val = strings.TrimSuffix(val, `"`)
		out[key] = val
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuotes = !inQuotes
		case ',':
			if !inQuotes {
				flush(i)
				start = i + 1
			}
		}
	}
	flush(len(s))
	return out
}

// buildDigestResponse computes an Authorization header value. If the
// challenge advertised qop="auth", the response uses the RFC 2617 form with
// cnonce/nc; otherwise the legacy no-qop form is used.
//
// The uri argument must match what the request line carries: an absolute
// RTSP URL for RTSP requests, or the request-target (path + query) for HTTP.
func buildDigestResponse(method, uri, user, pass string, c *digestChallenge) string {
	ha1 := md5hex(user + ":" + c.realm + ":" + pass)
	ha2 := md5hex(method + ":" + uri)

	parts := []string{
		fmt.Sprintf(`username="%s"`, user),
		fmt.Sprintf(`realm="%s"`, c.realm),
		fmt.Sprintf(`nonce="%s"`, c.nonce),
		fmt.Sprintf(`uri="%s"`, uri),
	}

	var response string
	if qop := pickQOP(c.qop); qop != "" {
		// We send exactly one request per challenge, so nc is always 1.
		const nc = "00000001"
		cnonce := randomCnonce()
		response = md5hex(ha1 + ":" + c.nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
		// qop and nc are sent UNQUOTED per RFC 2617; cnonce is quoted.
		// Order matches what curl --digest sends, which Dahua accepts.
		parts = append(parts,
			fmt.Sprintf(`cnonce="%s"`, cnonce),
			"nc="+nc,
			"qop="+qop,
		)
	} else {
		response = md5hex(ha1 + ":" + c.nonce + ":" + ha2)
	}

	parts = append(parts, fmt.Sprintf(`response="%s"`, response))
	if c.opaque != "" {
		parts = append(parts, fmt.Sprintf(`opaque="%s"`, c.opaque))
	}
	return "Digest " + strings.Join(parts, ", ")
}

// pickQOP returns "auth" if the challenge advertised it (the only qop value
// we support), or "" if no qop was advertised. Servers that advertise only
// "auth-int" (which would require hashing the body) are unsupported and
// fall through to the legacy no-qop form, which they will reject.
func pickQOP(qopList string) string {
	if qopList == "" {
		return ""
	}
	for p := range strings.SplitSeq(qopList, ",") {
		if strings.TrimSpace(p) == "auth" {
			return "auth"
		}
	}
	return ""
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomCnonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
