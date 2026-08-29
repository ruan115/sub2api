package worker

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

const activationPackageHeader = 44

var activationPackageMagic = [6]byte{'S', '2', 'A', 'P', '0', '1'}

// ActivationPackage is sealed as one unit to the worker process. In
// particular, authenticating the orchestrator rotation recipient together
// with the onboarding material prevents a host-agent from substituting a key
// that it controls and decrypting the normalized credential on the return
// path.
type ActivationPackage struct {
	Input                      OnboardingInput
	RotationRecipientKeyID     string
	RotationRecipientPublicKey []byte
}

func (p ActivationPackage) String() string {
	return fmt.Sprintf("ActivationPackage{Source:%q AuthType:%q RotationRecipientKeyID:%q OnboardingMaterial:[REDACTED] RotationRecipientPublicKey:[PUBLIC]}",
		p.Input.Source, p.Input.AuthType, p.RotationRecipientKeyID)
}

func (p ActivationPackage) GoString() string { return p.String() }

func (p ActivationPackage) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Source                 OnboardingSource `json:"source"`
		AuthType               string           `json:"auth_type"`
		RotationRecipientKeyID string           `json:"rotation_recipient_key_id"`
	}{p.Input.Source, p.Input.AuthType, p.RotationRecipientKeyID})
}

func (p *ActivationPackage) Destroy() {
	if p == nil {
		return
	}
	p.Input.Destroy()
	zero(p.RotationRecipientPublicKey)
	p.RotationRecipientPublicKey = nil
	p.RotationRecipientKeyID = ""
}

func (p ActivationPackage) Validate() error {
	if err := p.Input.Validate(); err != nil {
		return errors.New("activation package is invalid")
	}
	if credential.ValidateRecipientKey(p.RotationRecipientKeyID, p.RotationRecipientPublicKey) != nil {
		return errors.New("activation package is invalid")
	}
	return nil
}

func EncodeActivationPackage(pkg ActivationPackage) ([]byte, error) {
	if err := pkg.Validate(); err != nil {
		return nil, err
	}
	input, err := EncodeOnboardingInput(pkg.Input)
	if err != nil {
		return nil, errors.New("activation package cannot be encoded")
	}
	defer zero(input)
	keyIDLength := len(pkg.RotationRecipientKeyID)
	if keyIDLength > 128 || len(input) > maxOnboardingMaterial+onboardingWireHeader {
		return nil, errors.New("activation package is too large")
	}
	payload := make([]byte, activationPackageHeader+keyIDLength+len(input))
	copy(payload[:6], activationPackageMagic[:])
	binary.BigEndian.PutUint16(payload[6:8], uint16(keyIDLength))
	binary.BigEndian.PutUint32(payload[8:12], uint32(len(input)))
	copy(payload[12:44], pkg.RotationRecipientPublicKey)
	copy(payload[activationPackageHeader:], pkg.RotationRecipientKeyID)
	copy(payload[activationPackageHeader+keyIDLength:], input)
	return payload, nil
}

func DecodeActivationPackage(payload []byte) (ActivationPackage, error) {
	maxLength := activationPackageHeader + 128 + maxOnboardingMaterial + onboardingWireHeader
	if len(payload) <= activationPackageHeader || len(payload) > maxLength || !bytes.Equal(payload[:6], activationPackageMagic[:]) {
		return ActivationPackage{}, errors.New("activation package is invalid")
	}
	keyIDLength := int(binary.BigEndian.Uint16(payload[6:8]))
	inputLength := int(binary.BigEndian.Uint32(payload[8:12]))
	if keyIDLength <= 0 || keyIDLength > 128 || inputLength <= 0 || activationPackageHeader+keyIDLength+inputLength != len(payload) {
		return ActivationPackage{}, errors.New("activation package is invalid")
	}
	pkg := ActivationPackage{
		RotationRecipientKeyID:     string(payload[activationPackageHeader : activationPackageHeader+keyIDLength]),
		RotationRecipientPublicKey: append([]byte(nil), payload[12:44]...),
	}
	input, err := DecodeOnboardingInput(payload[activationPackageHeader+keyIDLength:])
	if err != nil {
		pkg.Destroy()
		return ActivationPackage{}, errors.New("activation package is invalid")
	}
	pkg.Input = input
	if err := pkg.Validate(); err != nil {
		pkg.Destroy()
		return ActivationPackage{}, err
	}
	return pkg, nil
}
