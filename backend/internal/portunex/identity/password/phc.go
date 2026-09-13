package password

import (
	"encoding/base64"
	"strconv"
	"strings"
)

type parameters struct {
	memory      uint32
	time        uint32
	parallelism uint8
	salt        []byte
	hash        []byte
}

// parse accepts only Argon2id v19 with exactly m, t and p. Parameter order is
// immaterial; duplicate/unknown keys, noncanonical integers and Base64 are not.
// It validates all resource parameters before decoding or starting a KDF.
func parse(encoded string, limits Limits) (parameters, error) {
	var result parameters
	if len(encoded) > limits.MaxPHCBytes {
		return result, ErrHashPolicy
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return result, ErrInvalidHash
	}
	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return result, ErrInvalidHash
	}
	var seen uint8
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return parameters{}, ErrInvalidHash
		}
		n, ok := positiveDecimal(value)
		if !ok {
			return parameters{}, ErrInvalidHash
		}
		var bit uint8
		switch key {
		case "m":
			bit, result.memory = 1, n
		case "t":
			bit, result.time = 2, n
		case "p":
			if n > 255 {
				return parameters{}, ErrInvalidHash
			}
			bit, result.parallelism = 4, uint8(n)
		default:
			return parameters{}, ErrInvalidHash
		}
		if seen&bit != 0 {
			return parameters{}, ErrInvalidHash
		}
		seen |= bit
	}
	if seen != 7 || result.memory < 8*uint32(result.parallelism) {
		return parameters{}, ErrInvalidHash
	}
	if result.memory > limits.MaxMemoryKiB || result.time > limits.MaxTime ||
		result.parallelism > limits.MaxParallelism ||
		uint64(result.memory)*uint64(result.time) > limits.MaxWorkKiB {
		return parameters{}, ErrHashPolicy
	}
	var err error
	result.salt, err = decodePart(parts[4], limits.MinSaltBytes, limits.MaxSaltBytes)
	if err != nil {
		return parameters{}, err
	}
	result.hash, err = decodePart(parts[5], limits.MinHashBytes, limits.MaxHashBytes)
	if err != nil {
		return parameters{}, err
	}
	return result, nil
}

func positiveDecimal(value string) (uint32, bool) {
	if len(value) == 0 || len(value) > 10 || value[0] == '0' {
		return 0, false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(value, 10, 32)
	return uint32(n), err == nil
}

func decodePart(encoded string, minimum, maximum int) ([]byte, error) {
	// uint64 avoids overflow in encoded-length arithmetic for caller-provided
	// limits. The input is already bounded by MaxPHCBytes.
	maxEncoded := uint64(maximum)/3*4 + (uint64(maximum)%3*8+5)/6
	if uint64(len(encoded)) > maxEncoded {
		return nil, ErrHashPolicy
	}
	for i := range len(encoded) {
		c := encoded[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '+' || c == '/') {
			return nil, ErrInvalidHash
		}
	}
	decoded, err := base64.RawStdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, ErrInvalidHash
	}
	if len(decoded) < minimum || len(decoded) > maximum {
		return nil, ErrHashPolicy
	}
	return decoded, nil
}
