package password

import (
	"context"
	"strings"
	"testing"
)

func FuzzParse(f *testing.F) {
	for _, encoded := range []string{
		vectorPHC(), "", "$argon2id$v=19$m=0,t=0,p=0$$", "$argon2id$v=16$m=64,t=1,p=1$$",
		strings.Repeat("$", testLimits().MaxPHCBytes+1),
		phcWith("m=4294967295,t=4294967295,p=255", []byte("somesalt"), vectorHash()),
	} {
		f.Add(encoded)
	}
	f.Fuzz(func(t *testing.T, encoded string) {
		limits := testLimits()
		params, err := parse(encoded, limits)
		if err != nil {
			return
		}
		if params.memory < 8*uint32(params.parallelism) || params.memory > limits.MaxMemoryKiB ||
			params.time == 0 || params.time > limits.MaxTime || params.parallelism == 0 || params.parallelism > limits.MaxParallelism ||
			uint64(params.memory)*uint64(params.time) > limits.MaxWorkKiB ||
			len(params.salt) < limits.MinSaltBytes || len(params.salt) > limits.MaxSaltBytes ||
			len(params.hash) < limits.MinHashBytes || len(params.hash) > limits.MaxHashBytes {
			t.Fatal("parser accepted parameters outside policy")
		}
	})
}

func FuzzVerifierAdmission(f *testing.F) {
	f.Add("password", vectorPHC())
	f.Add("", "")
	f.Add(strings.Repeat("x", testLimits().MaxPasswordBytes+1), vectorPHC())
	f.Fuzz(func(t *testing.T, password, encoded string) {
		limits := testLimits()
		v := newTestVerifier(t, limits)
		v.derive = func(value, salt []byte, passes, memory uint32, threads uint8, keyLen uint32) []byte {
			if len(value) > limits.MaxPasswordBytes || len(salt) > limits.MaxSaltBytes ||
				memory > limits.MaxMemoryKiB || passes > limits.MaxTime || threads > limits.MaxParallelism ||
				uint64(memory)*uint64(passes) > limits.MaxWorkKiB || keyLen > uint32(limits.MaxHashBytes) {
				t.Fatal("unsafe input reached KDF")
			}
			return make([]byte, keyLen)
		}
		_ = v.Verify(context.Background(), password, encoded)
		if !v.slots.TryAcquire(int64(limits.MaxConcurrent)) {
			t.Fatal("verification leaked admission capacity")
		}
		v.slots.Release(int64(limits.MaxConcurrent))
	})
}
