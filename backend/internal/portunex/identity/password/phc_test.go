package password

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// Public golang.org/x/crypto v0.53.0 argon2/argon2_test.go TestVectors:
// password="password", salt="somesalt", t=1, m=64 KiB, p=1, output=24 bytes.
// No fixture in this package is an actual account password hash.
const vectorHex = "655ad15eac652dc59f7170a7332bf49b8469be1fdb9c28bb"

func vectorHash() []byte {
	value, err := hex.DecodeString(vectorHex)
	if err != nil {
		panic("invalid public test fixture")
	}
	return value
}

func phcWith(parameters string, salt, hash []byte) string {
	return "$argon2id$v=19$" + parameters + "$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash)
}

func vectorPHC() string {
	return phcWith("m=64,t=1,p=1", []byte("somesalt"), vectorHash())
}

func TestParseAcceptsExactFormatAndReorderedParameters(t *testing.T) {
	for _, parameters := range []string{"m=64,t=1,p=1", "p=1,m=64,t=1", "t=1,p=1,m=64"} {
		got, err := parse(phcWith(parameters, []byte("somesalt"), vectorHash()), testLimits())
		if err != nil {
			t.Fatal(err)
		}
		if got.memory != 64 || got.time != 1 || got.parallelism != 1 || string(got.salt) != "somesalt" || hex.EncodeToString(got.hash) != vectorHex {
			t.Fatal("parsed fields differ from public vector")
		}
	}
	got, err := parse(phcWith("m=9,t=1,p=1", []byte("somesalt"), vectorHash()), testLimits())
	if err != nil || got.memory != 9 {
		t.Fatal("parser must not normalize the declared memory parameter")
	}
}

func TestParseRejectsMalformedFormat(t *testing.T) {
	valid := vectorPHC()
	cases := map[string]string{
		"empty": "", "missing prefix": strings.TrimPrefix(valid, "$"),
		"argon2i":                   strings.Replace(valid, "argon2id", "argon2i", 1),
		"argon2d":                   strings.Replace(valid, "argon2id", "argon2d", 1),
		"bcrypt":                    "$2a$10$not-an-accepted-format",
		"old version":               strings.Replace(valid, "v=19", "v=16", 1),
		"version leading zero":      strings.Replace(valid, "v=19", "v=019", 1),
		"missing version":           strings.Replace(valid, "$v=19", "", 1),
		"extra component":           valid + "$extra",
		"leading whitespace":        " " + valid,
		"salt padding":              strings.Replace(valid, "c29tZXNhbHQ", "c29tZXNhbHQ=", 1),
		"salt nonzero padding bits": strings.Replace(valid, "c29tZXNhbHQ", "c29tZXNhbHR", 1),
		"salt LF":                   strings.Replace(valid, "c29tZXNhbHQ", "c29t\nZXNhbHQ", 1),
		"salt CR":                   strings.Replace(valid, "c29tZXNhbHQ", "c29t\rZXNhbHQ", 1),
		"salt space":                strings.Replace(valid, "c29tZXNhbHQ", "c29t ZXNhbHQ", 1),
		"salt URL alphabet":         strings.Replace(valid, "c29tZXNhbHQ", "c29tZXNhbH_", 1),
		"salt illegal length":       strings.Replace(valid, "c29tZXNhbHQ", "A", 1),
		"hash LF":                   valid + "\n",
		"hash padding":              valid + "=",
		"hash illegal character":    valid + ".",
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(encoded, testLimits()); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("got %v, want invalid hash", err)
			}
		})
	}
}

func TestParseRejectsMalformedParameters(t *testing.T) {
	for _, parameters := range []string{
		"m=64,t=1", "m=64,t=1,p=1,x=1", "m=64,t=1,t=1", "m=64,x=1,p=1",
		"m=64,t=1,p=1,", "m=64,t=1,p", "m=64,t=1,p==1",
		"m=064,t=1,p=1", "m=+64,t=1,p=1", "m=-64,t=1,p=1", "m= 64,t=1,p=1",
		"m=64,t=1,p=１", "m=64,t=1,p=0", "m=64,t=0,p=1", "m=0,t=1,p=1",
		"m=8,t=1,p=2", "m=64,t=1,p=256", "m=4294967296,t=1,p=1",
		"m=64,t=4294967296,p=1", "m=18446744073709551616,t=1,p=1",
	} {
		t.Run(parameters, func(t *testing.T) {
			if _, err := parse(phcWith(parameters, []byte("somesalt"), vectorHash()), testLimits()); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("got %v, want invalid hash", err)
			}
		})
	}
}

func TestParseRejectsPolicyBeforeKDF(t *testing.T) {
	for name, encoded := range map[string]string{
		"PHC length":            strings.Repeat("x", testLimits().MaxPHCBytes+1),
		"memory":                phcWith("m=2048,t=1,p=1", []byte("somesalt"), vectorHash()),
		"memory uint32 maximum": phcWith("m=4294967295,t=1,p=1", []byte("somesalt"), vectorHash()),
		"passes":                phcWith("m=64,t=5,p=1", []byte("somesalt"), vectorHash()),
		"parallelism":           phcWith("m=64,t=1,p=8", []byte("somesalt"), vectorHash()),
		"salt minimum":          phcWith("m=64,t=1,p=1", []byte("short"), vectorHash()),
		"salt maximum":          phcWith("m=64,t=1,p=1", make([]byte, 65), vectorHash()),
		"hash minimum":          phcWith("m=64,t=1,p=1", []byte("somesalt"), make([]byte, 15)),
		"hash maximum":          phcWith("m=64,t=1,p=1", []byte("somesalt"), make([]byte, 65)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(encoded, testLimits()); !errors.Is(err, ErrHashPolicy) {
				t.Fatalf("got %v, want hash policy", err)
			}
		})
	}
	limits := testLimits()
	limits.MaxWorkKiB = 127
	if _, err := parse(phcWith("m=64,t=2,p=1", []byte("somesalt"), vectorHash()), limits); !errors.Is(err, ErrHashPolicy) {
		t.Fatalf("combined work limit: %v", err)
	}
}

func TestParsePolicyBoundaries(t *testing.T) {
	limits := testLimits()
	limits.MaxMemoryKiB, limits.MaxTime, limits.MaxParallelism, limits.MaxWorkKiB = 64, 1, 1, 64
	limits.MinSaltBytes, limits.MaxSaltBytes = 8, 8
	limits.MinHashBytes, limits.MaxHashBytes = 24, 24
	limits.MaxPHCBytes = len(vectorPHC())
	if _, err := parse(vectorPHC(), limits); err != nil {
		t.Fatalf("inclusive limits rejected: %v", err)
	}
	limits.MaxPHCBytes--
	if _, err := parse(vectorPHC(), limits); !errors.Is(err, ErrHashPolicy) {
		t.Fatalf("PHC byte boundary: %v", err)
	}
}

func TestDecodePartBoundsDoNotOverflow(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	value, err := decodePart("c29tZXNhbHQ", 8, maximum)
	if err != nil || string(value) != "somesalt" {
		t.Fatalf("overflow in encoded-length bound: %v", err)
	}
}
