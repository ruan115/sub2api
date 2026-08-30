package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"sort"
)

const (
	runtimeOnboardingCreateFingerprintVersion int64 = 1
	runtimeOnboardingCreateFingerprintDomain        = "ccmax.runtime-onboarding-create.v1\x00"
	runtimeOnboardingFingerprintSize                = sha256.Size
)

// runtimeOnboardingCreateFingerprint binds an account-create idempotency key
// only to normalized, typed, non-secret account configuration. In particular,
// it must never receive credential material, proxy text, free-form metadata, or
// values derived from any of those fields.
func runtimeOnboardingCreateFingerprint(
	input *accountInput,
	material *runtimeOnboardingMaterial,
	strategyID int64,
) ([sha256.Size]byte, error) {
	if input == nil || material == nil || input.Schedulable == nil ||
		input.Platform == "" || material.Source == "" || material.AuthType == "" {
		return [sha256.Size]byte{}, errRuntimeOnboardingIdempotency
	}
	rateMultiplier, err := canonicalRuntimeOnboardingFloat(input.RateMultiplier)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	accountPrice, err := canonicalRuntimeOnboardingFloat(input.AccountPrice)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	if strategyID < 0 {
		return [sha256.Size]byte{}, errRuntimeOnboardingIdempotency
	}
	groups := append([]string(nil), input.GroupIDs...)
	sort.Strings(groups)

	var canonical bytes.Buffer
	canonical.WriteString(runtimeOnboardingCreateFingerprintDomain)
	writeRuntimeOnboardingString(&canonical, runtimeOnboardingOperationCreate)
	writeRuntimeOnboardingString(&canonical, input.Platform)
	writeRuntimeOnboardingString(&canonical, material.Source)
	writeRuntimeOnboardingString(&canonical, material.AuthType)
	writeRuntimeOnboardingString(&canonical, input.Status)
	writeRuntimeOnboardingInt64(&canonical, int64(input.Concurrency))
	writeRuntimeOnboardingInt64(&canonical, int64(input.Priority))
	writeRuntimeOnboardingUint64(&canonical, rateMultiplier)
	writeRuntimeOnboardingInt64(&canonical, int64(input.BaseRPM))
	writeRuntimeOnboardingString(&canonical, input.RPMStrategy)
	writeRuntimeOnboardingInt64(&canonical, int64(input.RPMStickyBuffer))
	writeRuntimeOnboardingString(&canonical, input.UserMsgQueueMode)
	writeRuntimeOnboardingUint64(&canonical, accountPrice)
	writeRuntimeOnboardingBool(&canonical, boolPointerValue(input.Quota5HThresholdEnabled, false))
	writeRuntimeOnboardingInt64(&canonical, int64(intPointerValue(input.Quota5HThresholdPercent, 80)))
	writeRuntimeOnboardingBool(&canonical, boolPointerValue(input.Quota7DThresholdEnabled, false))
	writeRuntimeOnboardingInt64(&canonical, int64(intPointerValue(input.Quota7DThresholdPercent, 80)))
	writeRuntimeOnboardingUint64(&canonical, uint64(len(groups)))
	for _, groupID := range groups {
		writeRuntimeOnboardingString(&canonical, groupID)
	}
	writeRuntimeOnboardingInt64(&canonical, runtimeOnboardingPointerID(input.ProxyPoolID))
	writeRuntimeOnboardingBool(&canonical, input.AutoProxy)
	writeRuntimeOnboardingInt64(&canonical, runtimeOnboardingPointerID(input.ProxyID))
	writeRuntimeOnboardingInt64(&canonical, strategyID)
	return sha256.Sum256(canonical.Bytes()), nil
}

func canonicalRuntimeOnboardingFloat(value float64) (uint64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, errors.New("invalid account numeric field")
	}
	if value == 0 {
		value = 0 // Collapse negative zero into the canonical positive zero.
	}
	return math.Float64bits(value), nil
}

func runtimeOnboardingPointerID(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func writeRuntimeOnboardingString(target *bytes.Buffer, value string) {
	writeRuntimeOnboardingUint64(target, uint64(len(value)))
	target.WriteString(value)
}

func writeRuntimeOnboardingInt64(target *bytes.Buffer, value int64) {
	writeRuntimeOnboardingUint64(target, uint64(value))
}

func writeRuntimeOnboardingUint64(target *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	target.Write(encoded[:])
}

func writeRuntimeOnboardingBool(target *bytes.Buffer, value bool) {
	if value {
		target.WriteByte(1)
		return
	}
	target.WriteByte(0)
}

func runtimeOnboardingRequestedStrategyID(value *int64) int64 {
	if value == nil || *value <= 0 {
		return 0
	}
	return *value
}

// validateRuntimeOnboardingProxySelection validates only the stable,
// non-secret shape that is safe to evaluate before an idempotency replay.
func validateRuntimeOnboardingProxySelection(poolID, requestedProxyID *int64, auto bool) error {
	if poolID == nil || *poolID <= 0 {
		return errors.New("execution onboarding requires an active proxy pool")
	}
	if requestedProxyID != nil && *requestedProxyID <= 0 {
		return errors.New("execution onboarding proxy id is invalid")
	}
	if !auto && requestedProxyID == nil {
		return errors.New("execution onboarding requires a proxy or automatic matching")
	}
	return nil
}

// validateRuntimeOnboardingProxyAvailability runs only after an idempotency
// key has no durable owner. Mutable proxy state must not prevent an exact
// replay after the first request has committed its account.
func (a *app) validateRuntimeOnboardingProxyAvailability(poolID, requestedProxyID *int64) error {
	if a == nil || a.db == nil || poolID == nil || *poolID <= 0 {
		return errors.New("execution onboarding requires an active proxy pool")
	}
	var poolExists int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxy_pools
		WHERE id = ? AND status = 'active' AND deleted_at IS NULL AND system_kind = ''`, *poolID).Scan(&poolExists); err != nil {
		return err
	}
	if poolExists != 1 {
		return errors.New("execution onboarding proxy pool is unavailable")
	}
	if requestedProxyID == nil {
		return nil
	}
	var proxyExists int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxies
		WHERE id = ? AND pool_id = ? AND status = 'active' AND deleted_at IS NULL`, *requestedProxyID, *poolID).Scan(&proxyExists); err != nil {
		return err
	}
	if proxyExists != 1 {
		return errors.New("execution onboarding proxy does not belong to the selected pool or is unavailable")
	}
	return nil
}
