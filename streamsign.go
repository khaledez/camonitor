package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// signStream returns "<expUnix>.<base64url HMAC(camera|expUnix)>".
func signStream(secret []byte, camera string, exp time.Time) string {
	expUnix := strconv.FormatInt(exp.Unix(), 10)
	return expUnix + "." + base64.RawURLEncoding.EncodeToString(streamMAC(secret, camera, expUnix))
}

func verifyStream(secret []byte, camera, token string, now time.Time) bool {
	expUnix, sig, found := strings.Cut(token, ".")
	exp, err := strconv.ParseInt(expUnix, 10, 64)
	gotMAC, decodeErr := base64.RawURLEncoding.DecodeString(sig)
	if !found || err != nil || decodeErr != nil {
		return false
	}
	isSignatureValid := hmac.Equal(gotMAC, streamMAC(secret, camera, expUnix))
	isFresh := now.Unix() <= exp
	return isSignatureValid && isFresh
}

func streamMAC(secret []byte, camera, expUnix string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(camera + "|" + expUnix))
	return mac.Sum(nil)
}
