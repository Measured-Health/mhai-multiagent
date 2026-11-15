// Package s3 is the documents service's object store: a small S3 client
// signed with AWS Signature Version 4, the browser's POST policies (§4.2)
// and the bucket lifecycle check (§5.4, §6.1). It reaches one bucket with
// the documents service's own credential (§6.1: nobody else holds one) and
// touches only the three prefixes the design names, each object key made
// of ids (§6.1).
//
// Nothing here logs. Errors are closed sentinels or carry an HTTP status
// code only: never a key's secret, a signature, a policy, a URL with a
// query or a response body.
package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Algorithm is the SigV4 algorithm name.
const Algorithm = "AWS4-HMAC-SHA256"

// service is the SigV4 service name.
const service = "s3"

// Credentials are the documents service's object store credential.
type Credentials struct {
	AccessKeyID string
	// Secret is the secret access key, read from its file at start.
	Secret string
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// signingKey is SigV4's derived key for date (YYYYMMDD) and region.
func signingKey(secret, date, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// scope is date/region/s3/aws4_request.
func scope(date, region string) string {
	return date + "/" + region + "/" + service + "/aws4_request"
}

// amzDate is t as SigV4's X-Amz-Date (UTC, basic ISO 8601).
func amzDate(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

func shortDate(t time.Time) string { return t.UTC().Format("20060102") }
