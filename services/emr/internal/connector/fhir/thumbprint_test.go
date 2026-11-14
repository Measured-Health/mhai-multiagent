package fhir

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/hex"
	"testing"
)

// The RFC 7638 thumbprint of fixed, synthetic P-384 keys. The expected
// values were computed outside Go: the public point by plain scalar
// multiplication, then the base64url SHA-256 of the canonical JWK
// {"crv":"P-384","kty":"EC","x":...,"y":...} with x and y left-padded to
// 48 bytes. The second key's X starts with a zero byte (47 significant
// bytes), so its thumbprint is only right if the padding is kept.
func TestThumbprintP384(t *testing.T) {
	cases := []struct {
		name, d, want string
		xLeadingZero  bool
	}{
		{
			name: "full-length X",
			d:    "f02ce3cca2ced84c26cc26cdf75624e57f38bfedf4d6767309ae5fcc0349e7cf3bd3c9f50d5d47fe1766ac076bba9c88",
			want: "-11V6ZGdP0iVB8HLdNsfL9wytC1Bgsq8a8Syy3iGC1Y",
		},
		{
			name:         "X with a leading zero byte",
			d:            "e94aff8477b95805727e4235934093c67bb779f6535606795aeecfe1bcb9ad12933ee4714a2cca193c1c499d47cee6f9",
			want:         "mnx6Q-aAEWLbNZEN1Vf1JQNBjoNrZUl_JCgbMjyqNi0",
			xLeadingZero: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := hex.DecodeString(c.d)
			if err != nil {
				t.Fatal(err)
			}
			k, err := ecdsa.ParseRawPrivateKey(elliptic.P384(), d)
			if err != nil {
				t.Fatal(err)
			}
			pt, err := k.PublicKey.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			if got := pt[1] == 0; got != c.xLeadingZero {
				t.Fatalf("X leading zero byte = %v, want %v (the fixture key is wrong)", got, c.xLeadingZero)
			}
			if got := thumbprint(k.Public()); got != c.want {
				t.Fatalf("thumbprint = %s, want %s", got, c.want)
			}
		})
	}
}
