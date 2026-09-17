package runtimebootstrap

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

func DecodeRequest(data []byte) (Request, error) {
	if len(data) == 0 || len(data) > MaxPublicBytes {
		return Request{}, ErrBootstrap
	}
	data = bytes.TrimSpace(data)
	var request Request
	if json.Unmarshal(data, &request) != nil {
		return Request{}, ErrBootstrap
	}
	canonical, err := json.Marshal(request)
	if err != nil || !bytes.Equal(canonical, data) {
		return Request{}, ErrBootstrap
	}
	if _, err := runtimeidentity.ValidateCSR(request.Binding, request.CSRPEM); err != nil {
		return Request{}, ErrBootstrap
	}
	return request, nil
}

func publicCertificate(value []byte) bool {
	if len(value) == 0 || len(value) > 16*1024 {
		return false
	}
	value = bytes.TrimSpace(value)
	if !bytes.HasPrefix(value, []byte("-----BEGIN CERTIFICATE-----\n")) {
		return false
	}
	block, rest := pem.Decode(value)
	if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return false
	}
	_, err := x509.ParseCertificate(block.Bytes)
	return err == nil
}

func EncodeBundleArgument(bundle PublicBundle) (string, error) {
	if !publicCertificate(bundle.CertificatePEM) || !publicCertificate(bundle.CAPEM) {
		return "", ErrBootstrap
	}
	data, err := json.Marshal(bundle)
	if err != nil || len(data) > MaxPublicBytes {
		return "", ErrBootstrap
	}
	encoded := base64.RawStdEncoding.EncodeToString(data)
	if len(encoded) > MaxEncodedBundleBytes {
		return "", ErrBootstrap
	}
	return encoded, nil
}

func DecodeBundleArgument(encoded string) (PublicBundle, error) {
	if len(encoded) == 0 || len(encoded) > MaxEncodedBundleBytes {
		return PublicBundle{}, ErrBootstrap
	}
	data, err := base64.RawStdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) > MaxPublicBytes || base64.RawStdEncoding.EncodeToString(data) != encoded {
		return PublicBundle{}, ErrBootstrap
	}
	var bundle PublicBundle
	if json.Unmarshal(data, &bundle) != nil {
		return PublicBundle{}, ErrBootstrap
	}
	canonical, err := EncodeBundleArgument(bundle)
	if err != nil || canonical != encoded {
		return PublicBundle{}, ErrBootstrap
	}
	return bundle, nil
}
