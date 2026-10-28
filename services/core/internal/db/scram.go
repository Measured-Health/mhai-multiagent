package db

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// scramIterations is Postgres's default scram_iterations.
const scramIterations = 4096

// scramVerifier builds the SCRAM-SHA-256 verifier Postgres stores for
// password (RFC 5802, RFC 7677), with a fresh 16-byte salt:
//
//	SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>
//
// Postgres keeps a value in this form as it is, so the plaintext never
// reaches the server, its logs or pg_stat_statements. password must be
// printable ASCII (SASLprep leaves that unchanged).
func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("db: no randomness")
	}
	return scramVerifierSalt(password, salt, scramIterations)
}

func scramVerifierSalt(password string, salt []byte, iterations int) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", errors.New("db: scram key derivation failed")
	}
	mac := func(key []byte, msg string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(msg))
		return m.Sum(nil)
	}
	clientKey := mac(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	serverKey := mac(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return "SCRAM-SHA-256$" + strconv.Itoa(iterations) + ":" + b64(salt) + "$" + b64(stored[:]) + ":" + b64(serverKey), nil
}

// passwordStatement is the ALTER ROLE statement that sets the login's
// password. The name is quoted as an identifier; the password goes only as
// its verifier, whose characters (base64, digits, '$', ':', '-') need no
// escaping inside a literal. It names no attribute, so the only thing in it
// that can fail is the privilege EnsureLogin has already probed.
func passwordStatement(name, password string) (string, error) {
	v, err := scramVerifier(password)
	if err != nil {
		return "", err
	}
	return "ALTER ROLE " + pgx.Identifier{name}.Sanitize() + " PASSWORD '" + v + "'", nil
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
