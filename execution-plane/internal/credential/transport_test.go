package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCredentialTransportRoundTripAndContextBinding(t *testing.T) {
	t.Parallel()
	recipient, err := NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x31}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	keyID, publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	binding := TransportContext{
		AccountBinding: "95a7c9f1f7654af7a836061a6561b839", SlotID: "slot-10380",
		ExecutionEpoch: 7, LeaseID: "credential-lease-7", ProxyLeaseID: "proxy-lease-7", Purpose: "onboarding",
	}
	plaintext := []byte(`{"source":"session_key","secret":"must-remain-secret"}`)
	sealed, err := SealForRecipient(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x72}, 128)), keyID, publicKey, binding, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("must-remain-secret")) {
		t.Fatal("sealed worker credential bundle contains plaintext")
	}
	for _, serialized := range []string{recipient.String(), fmt.Sprintf("%+v", recipient), string(mustMarshalTransport(t, recipient))} {
		if strings.Contains(serialized, "49 49 49") {
			t.Fatalf("recipient serialization leaked private key bytes: %s", serialized)
		}
	}
	opened, err := recipient.Open(context.Background(), sealed, binding)
	if err != nil {
		t.Fatal(err)
	}
	defer zeroBytes(opened)
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened credential = %q", opened)
	}

	for name, changed := range map[string]TransportContext{
		"account": {AccountBinding: "other-account", SlotID: binding.SlotID, ExecutionEpoch: binding.ExecutionEpoch, LeaseID: binding.LeaseID, ProxyLeaseID: binding.ProxyLeaseID, Purpose: binding.Purpose},
		"slot":    {AccountBinding: binding.AccountBinding, SlotID: "slot-other", ExecutionEpoch: binding.ExecutionEpoch, LeaseID: binding.LeaseID, ProxyLeaseID: binding.ProxyLeaseID, Purpose: binding.Purpose},
		"epoch":   {AccountBinding: binding.AccountBinding, SlotID: binding.SlotID, ExecutionEpoch: 8, LeaseID: binding.LeaseID, ProxyLeaseID: binding.ProxyLeaseID, Purpose: binding.Purpose},
		"lease":   {AccountBinding: binding.AccountBinding, SlotID: binding.SlotID, ExecutionEpoch: binding.ExecutionEpoch, LeaseID: "lease-other", ProxyLeaseID: binding.ProxyLeaseID, Purpose: binding.Purpose},
		"proxy":   {AccountBinding: binding.AccountBinding, SlotID: binding.SlotID, ExecutionEpoch: binding.ExecutionEpoch, LeaseID: binding.LeaseID, ProxyLeaseID: "proxy-other", Purpose: binding.Purpose},
		"purpose": {AccountBinding: binding.AccountBinding, SlotID: binding.SlotID, ExecutionEpoch: binding.ExecutionEpoch, LeaseID: binding.LeaseID, ProxyLeaseID: binding.ProxyLeaseID, Purpose: "activation"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := recipient.Open(context.Background(), sealed, changed); !errors.Is(err, ErrCredentialTransport) {
				t.Fatalf("changed context error = %v", err)
			}
		})
	}
}

func TestCredentialTransportRejectsTamperingWrongKeyAndUnknownFields(t *testing.T) {
	t.Parallel()
	recipient, _ := NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x22}, 32)))
	defer recipient.Destroy()
	other, _ := NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x23}, 32)))
	defer other.Destroy()
	keyID, publicKey, _ := recipient.PublicKey()
	binding := TransportContext{AccountBinding: "account", SlotID: "slot", ExecutionEpoch: 1, LeaseID: "lease", ProxyLeaseID: "proxy", Purpose: "activation"}
	sealed, err := SealForRecipient(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x45}, 128)), keyID, publicKey, binding, []byte("credential-secret"))
	if err != nil {
		t.Fatal(err)
	}

	var bundle map[string]any
	if err := json.Unmarshal(sealed, &bundle); err != nil {
		t.Fatal(err)
	}
	tamperedCiphertext := cloneJSONMap(t, bundle)
	ciphertext := tamperedCiphertext["ciphertext"].(string)
	if strings.HasSuffix(ciphertext, "A") {
		ciphertext = ciphertext[:len(ciphertext)-1] + "B"
	} else {
		ciphertext = ciphertext[:len(ciphertext)-1] + "A"
	}
	tamperedCiphertext["ciphertext"] = ciphertext
	tamperedContext := cloneJSONMap(t, bundle)
	tamperedContext["context"].(map[string]any)["slot_id"] = "changed-slot"
	unknown := cloneJSONMap(t, bundle)
	unknown["unexpected"] = true
	for name, candidate := range map[string][]byte{
		"ciphertext": mustMarshalTransport(t, tamperedCiphertext),
		"context":    mustMarshalTransport(t, tamperedContext),
		"unknown":    mustMarshalTransport(t, unknown),
		"trailing":   append(append([]byte(nil), sealed...), []byte(" {}")...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := recipient.Open(context.Background(), candidate, binding); !errors.Is(err, ErrCredentialTransport) {
				t.Fatalf("tampered bundle error = %v", err)
			}
		})
	}
	if _, err := other.Open(context.Background(), sealed, binding); !errors.Is(err, ErrCredentialTransport) {
		t.Fatalf("wrong recipient error = %v", err)
	}
	otherKeyID, _, _ := other.PublicKey()
	if _, err := SealForRecipient(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x41}, 128)), otherKeyID, publicKey, binding, []byte("secret")); !errors.Is(err, ErrCredentialTransport) {
		t.Fatalf("mismatched public key id error = %v", err)
	}
}

func TestCredentialTransportConcurrentOpenAndDestroy(t *testing.T) {
	recipient, _ := NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x61}, 32)))
	keyID, publicKey, _ := recipient.PublicKey()
	binding := TransportContext{AccountBinding: "account", SlotID: "slot", ExecutionEpoch: 2, LeaseID: "lease", ProxyLeaseID: "proxy", Purpose: "rotation"}
	sealed, _ := SealForRecipient(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x62}, 128)), keyID, publicKey, binding, []byte("rotation-secret"))
	var wait sync.WaitGroup
	for index := 0; index < 32; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			plaintext, err := recipient.Open(context.Background(), sealed, binding)
			if err == nil {
				zeroBytes(plaintext)
			} else if !errors.Is(err, ErrCredentialTransport) {
				t.Errorf("unexpected concurrent open error: %v", err)
			}
		}()
	}
	recipient.Destroy()
	wait.Wait()
	if _, _, err := recipient.PublicKey(); !errors.Is(err, ErrCredentialTransport) {
		t.Fatalf("destroyed recipient public key error = %v", err)
	}
	if _, err := recipient.Open(context.Background(), sealed, binding); !errors.Is(err, ErrCredentialTransport) {
		t.Fatalf("destroyed recipient open error = %v", err)
	}
}

func TestRotationMaterialCodecRedactsAndErases(t *testing.T) {
	t.Parallel()
	material := RotationMaterial{AuthType: "oauth", Plaintext: []byte(`{"access_token":"rotation-secret"}`)}
	payload, err := EncodeRotationMaterial(material)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRotationMaterial(payload)
	if err != nil {
		t.Fatal(err)
	}
	alias := decoded.Plaintext
	if !bytes.Equal(decoded.Plaintext, material.Plaintext) {
		t.Fatalf("decoded rotation material = %q", decoded.Plaintext)
	}
	for _, serialized := range []string{decoded.String(), fmt.Sprintf("%+v", decoded), string(mustMarshalTransport(t, decoded))} {
		if strings.Contains(serialized, "rotation-secret") {
			t.Fatalf("rotation material serialization leaked plaintext: %s", serialized)
		}
	}
	decoded.Destroy()
	for _, value := range alias {
		if value != 0 {
			t.Fatal("rotation material destroy did not erase plaintext")
		}
	}
	for _, candidate := range [][]byte{payload[:5], append(append([]byte(nil), payload...), 0), func() []byte {
		changed := append([]byte(nil), payload...)
		changed[6] = 99
		return changed
	}()} {
		if decoded, err := DecodeRotationMaterial(candidate); err == nil {
			decoded.Destroy()
			t.Fatalf("tampered rotation material decoded: %x", candidate)
		}
	}
}

func TestCredentialTransportAllowsBoundedCredentialFramingOverhead(t *testing.T) {
	recipient, err := NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x7c}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	keyID, publicKey, _ := recipient.PublicKey()
	material := RotationMaterial{AuthType: "oauth", Plaintext: bytes.Repeat([]byte{'a'}, maxPlaintextBytes)}
	payload, err := EncodeRotationMaterial(material)
	if err != nil {
		t.Fatal(err)
	}
	defer zeroBytes(payload)
	binding := TransportContext{AccountBinding: "account", SlotID: "slot", ExecutionEpoch: 1, LeaseID: "lease", ProxyLeaseID: "proxy", Purpose: "rotation"}
	sealed, err := SealForRecipient(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x7d}, 128)), keyID, publicKey, binding, payload)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := recipient.Open(context.Background(), sealed, binding)
	if err != nil {
		t.Fatal(err)
	}
	defer zeroBytes(opened)
	if !bytes.Equal(opened, payload) {
		t.Fatal("transport changed maximum-sized framed credential")
	}
}

func cloneJSONMap(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	payload, _ := json.Marshal(source)
	var clone map[string]any
	if err := json.Unmarshal(payload, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func mustMarshalTransport(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
