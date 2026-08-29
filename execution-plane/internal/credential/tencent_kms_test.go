package credential

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	tencentkms "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/kms/v20190118"
)

func TestTencentKMSGenerateAndDecrypt(t *testing.T) {
	t.Parallel()
	dataKey := bytes.Repeat([]byte{0x4a}, dataKeySize)
	keyID := "93866e69-9755-11ef-8e65-52540089bc41"
	aad := []byte(`{"schema":"ccmax.credential.v1","account_id":"account-1"}`)
	api := &fakeTencentKMSAPI{}
	api.generate = func(_ context.Context, request *tencentkms.GenerateDataKeyRequest) (*tencentkms.GenerateDataKeyResponse, error) {
		if request.KeyId == nil || *request.KeyId != keyID || request.KeySpec == nil || *request.KeySpec != "AES_256" || request.NumberOfBytes == nil || *request.NumberOfBytes != dataKeySize {
			t.Fatalf("unexpected GenerateDataKey request: %+v", request)
		}
		if request.EncryptionContext == nil || *request.EncryptionContext != string(aad) || request.IsHostedByKms == nil || *request.IsHostedByKms != 0 {
			t.Fatalf("GenerateDataKey context/hosting mismatch: %+v", request)
		}
		plaintext := base64.StdEncoding.EncodeToString(dataKey)
		ciphertext := "kms-opaque-ciphertext"
		return &tencentkms.GenerateDataKeyResponse{Response: &tencentkms.GenerateDataKeyResponseParams{
			KeyId: &keyID, Plaintext: &plaintext, CiphertextBlob: &ciphertext,
		}}, nil
	}
	api.decrypt = func(_ context.Context, request *tencentkms.DecryptRequest) (*tencentkms.DecryptResponse, error) {
		if request.CiphertextBlob == nil || *request.CiphertextBlob != "kms-opaque-ciphertext" || request.EncryptionContext == nil || *request.EncryptionContext != string(aad) {
			t.Fatalf("unexpected Decrypt request: %+v", request)
		}
		plaintext := base64.StdEncoding.EncodeToString(dataKey)
		return &tencentkms.DecryptResponse{Response: &tencentkms.DecryptResponseParams{KeyId: &keyID, Plaintext: &plaintext}}, nil
	}

	provider, err := NewTencentKMS(TencentKMSConfig{Region: "ap-guangzhou", KeyID: keyID, KeyVersion: "cmk-current"}, api)
	if err != nil {
		t.Fatalf("new Tencent KMS: %v", err)
	}
	generated, err := provider.GenerateDataKey(context.Background(), aad)
	if err != nil {
		t.Fatalf("generate data key: %v", err)
	}
	defer zeroBytes(generated.Plaintext)
	if !bytes.Equal(generated.Plaintext, dataKey) || string(generated.Wrapped.Ciphertext) != "kms-opaque-ciphertext" || generated.Wrapped.KeyVersion != "cmk-current" {
		t.Fatalf("unexpected generated data key: %+v", generated.Wrapped)
	}
	decrypted, err := provider.DecryptDataKey(context.Background(), generated.Wrapped, aad)
	if err != nil {
		t.Fatalf("decrypt data key: %v", err)
	}
	defer zeroBytes(decrypted)
	if !bytes.Equal(decrypted, dataKey) {
		t.Fatal("decrypted data key mismatch")
	}
}

func TestTencentKMSFailsClosedOnMalformedOrFailedResponses(t *testing.T) {
	t.Parallel()
	keyID := "kms-test"
	config := TencentKMSConfig{Region: "ap-guangzhou", KeyID: keyID, KeyVersion: "v1"}
	api := &fakeTencentKMSAPI{
		generate: func(context.Context, *tencentkms.GenerateDataKeyRequest) (*tencentkms.GenerateDataKeyResponse, error) {
			return nil, errors.New("SDK failure with sensitive-looking text")
		},
		decrypt: func(context.Context, *tencentkms.DecryptRequest) (*tencentkms.DecryptResponse, error) {
			wrongLength := base64.StdEncoding.EncodeToString([]byte("short"))
			return &tencentkms.DecryptResponse{Response: &tencentkms.DecryptResponseParams{KeyId: &keyID, Plaintext: &wrongLength}}, nil
		},
	}
	provider, err := NewTencentKMS(config, api)
	if err != nil {
		t.Fatalf("new Tencent KMS: %v", err)
	}
	if _, err := provider.GenerateDataKey(context.Background(), []byte("aad")); !errors.Is(err, ErrKMSUnavailable) || err.Error() != ErrKMSUnavailable.Error() {
		t.Fatalf("GenerateDataKey() error = %v, want sanitized ErrKMSUnavailable", err)
	}
	wrapped := WrappedDataKey{Ciphertext: []byte("opaque"), KeyID: keyID, KeyVersion: "v1"}
	if _, err := provider.DecryptDataKey(context.Background(), wrapped, []byte("aad")); !errors.Is(err, ErrKMSUnavailable) {
		t.Fatalf("DecryptDataKey() error = %v, want ErrKMSUnavailable", err)
	}
	wrapped.KeyVersion = "old"
	if _, err := provider.DecryptDataKey(context.Background(), wrapped, []byte("aad")); !errors.Is(err, ErrKMSUnavailable) {
		t.Fatalf("DecryptDataKey(wrong version) error = %v, want ErrKMSUnavailable", err)
	}
}

func TestTencentKMSConfigValidation(t *testing.T) {
	t.Parallel()
	api := &fakeTencentKMSAPI{}
	tests := []TencentKMSConfig{
		{},
		{Region: "ap-guangzhou", KeyID: "kms-test", KeyVersion: "v1", Endpoint: "https://kms.tencentcloudapi.com"},
		{Region: "ap-guangzhou", KeyID: "kms-test", KeyVersion: "v1", Endpoint: "kms.tencentcloudapi.com/path"},
		{Region: "ap-guangzhou", KeyID: "kms-test", KeyVersion: "v1", TimeoutSeconds: 61},
	}
	for _, config := range tests {
		if _, err := NewTencentKMS(config, api); err == nil {
			t.Fatalf("NewTencentKMS(%+v) succeeded", config)
		}
	}
	valid := TencentKMSConfig{Region: "ap-guangzhou", KeyID: "kms-test", KeyVersion: "v1", Endpoint: "kms.tencentcloudapi.com", TimeoutSeconds: 5}
	if _, err := NewTencentKMS(valid, api); err != nil {
		t.Fatalf("NewTencentKMS(valid) error = %v", err)
	}
	if _, err := NewTencentKMSWithCredential(valid, nil); err == nil {
		t.Fatal("NewTencentKMSWithCredential(nil) succeeded")
	}
}

type fakeTencentKMSAPI struct {
	generate func(context.Context, *tencentkms.GenerateDataKeyRequest) (*tencentkms.GenerateDataKeyResponse, error)
	decrypt  func(context.Context, *tencentkms.DecryptRequest) (*tencentkms.DecryptResponse, error)
}

func (f *fakeTencentKMSAPI) GenerateDataKeyWithContext(ctx context.Context, request *tencentkms.GenerateDataKeyRequest) (*tencentkms.GenerateDataKeyResponse, error) {
	if f.generate == nil {
		return nil, errors.New("generate not configured")
	}
	return f.generate(ctx, request)
}

func (f *fakeTencentKMSAPI) DecryptWithContext(ctx context.Context, request *tencentkms.DecryptRequest) (*tencentkms.DecryptResponse, error) {
	if f.decrypt == nil {
		return nil, errors.New("decrypt not configured")
	}
	return f.decrypt(ctx, request)
}
