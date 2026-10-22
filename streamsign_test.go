package main

import (
	"testing"
	"time"
)

func TestStreamSign(t *testing.T) {
	secret := []byte("s3cret")
	now := time.Unix(1_000_000, 0)
	token := signStream(secret, "front", now.Add(time.Minute))

	cases := []struct {
		name   string
		secret []byte
		camera string
		token  string
		at     time.Time
		want   bool
	}{
		{"valid", secret, "front", token, now, true},
		{"other camera", secret, "back", token, now, false},
		{"tampered sig", secret, "front", token[:len(token)-2] + "xx", now, false},
		{"expired", secret, "front", token, now.Add(2 * time.Minute), false},
		{"other secret", []byte("nope"), "front", token, now, false},
		{"garbage", secret, "front", "garbage", now, false},
	}
	for _, c := range cases {
		if got := verifyStream(c.secret, c.camera, c.token, c.at); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
