package credential

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	tencentkms "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/kms/v20190118"
)

const defaultTencentKMSTimeoutSeconds = 10

type TencentKMSConfig struct {
	Region         string
	KeyID          string
	KeyVersion     string
	Endpoint       string
	CVMRoleName    string
	TimeoutSeconds int
}

// TencentKMSAPI is the test seam around the official Tencent Cloud KMS API
// 3.0 client.
type TencentKMSAPI interface {
	GenerateDataKeyWithContext(context.Context, *tencentkms.GenerateDataKeyRequest) (*tencentkms.GenerateDataKeyResponse, error)
	DecryptWithContext(context.Context, *tencentkms.DecryptRequest) (*tencentkms.DecryptResponse, error)
}

type TencentKMS struct {
	api        TencentKMSAPI
	keyID      string
	keyVersion string
}

// NewTencentKMSFromCVMRole intentionally uses only the CVM instance-role
// provider. It never falls back to environment variables or credential files.
func NewTencentKMSFromCVMRole(config TencentKMSConfig) (*TencentKMS, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	provider := common.NewCvmRoleProvider(strings.TrimSpace(config.CVMRoleName))
	credential, err := provider.GetCredential()
	if err != nil {
		return nil, ErrKMSUnavailable
	}
	return NewTencentKMSWithCredential(config, credential)
}

func NewTencentKMSWithCredential(config TencentKMSConfig, credential common.CredentialIface) (*TencentKMS, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if credential == nil {
		return nil, errors.New("Tencent KMS credential is required")
	}
	clientProfile := profile.NewClientProfile()
	clientProfile.HttpProfile.Scheme = "HTTPS"
	clientProfile.HttpProfile.ReqTimeout = config.timeoutSeconds()
	if endpoint := strings.TrimSpace(config.Endpoint); endpoint != "" {
		clientProfile.HttpProfile.Endpoint = endpoint
	}
	client, err := tencentkms.NewClient(credential, strings.TrimSpace(config.Region), clientProfile)
	if err != nil {
		return nil, ErrKMSUnavailable
	}
	return NewTencentKMS(config, client)
}

func NewTencentKMS(config TencentKMSConfig, api TencentKMSAPI) (*TencentKMS, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if api == nil {
		return nil, errors.New("Tencent KMS API client is required")
	}
	return &TencentKMS{
		api:        api,
		keyID:      strings.TrimSpace(config.KeyID),
		keyVersion: strings.TrimSpace(config.KeyVersion),
	}, nil
}

func (k *TencentKMS) GenerateDataKey(ctx context.Context, aad []byte) (GeneratedDataKey, error) {
	if len(aad) == 0 || len(aad) > maxAADBytes {
		return GeneratedDataKey{}, ErrKMSUnavailable
	}
	keySpec := "AES_256"
	bytesRequested := uint64(dataKeySize)
	keyID := k.keyID
	encryptionContext := string(aad)
	notHosted := uint64(0)
	request := tencentkms.NewGenerateDataKeyRequest()
	request.KeyId = &keyID
	request.KeySpec = &keySpec
	request.NumberOfBytes = &bytesRequested
	request.EncryptionContext = &encryptionContext
	request.IsHostedByKms = &notHosted

	response, err := k.api.GenerateDataKeyWithContext(ctx, request)
	if err != nil || response == nil || response.Response == nil || response.Response.Plaintext == nil || response.Response.CiphertextBlob == nil || response.Response.KeyId == nil || *response.Response.KeyId != k.keyID {
		return GeneratedDataKey{}, ErrKMSUnavailable
	}
	plaintext, err := base64.StdEncoding.DecodeString(*response.Response.Plaintext)
	if err != nil || len(plaintext) != dataKeySize || *response.Response.CiphertextBlob == "" {
		zeroBytes(plaintext)
		return GeneratedDataKey{}, ErrKMSUnavailable
	}
	return GeneratedDataKey{
		Plaintext: plaintext,
		Wrapped: WrappedDataKey{
			Ciphertext: []byte(*response.Response.CiphertextBlob),
			KeyID:      k.keyID,
			KeyVersion: k.keyVersion,
		},
	}, nil
}

func (k *TencentKMS) DecryptDataKey(ctx context.Context, wrapped WrappedDataKey, aad []byte) ([]byte, error) {
	if wrapped.KeyID != k.keyID || wrapped.KeyVersion != k.keyVersion || len(wrapped.Ciphertext) == 0 || len(wrapped.Ciphertext) > maxWrappedKeySize || len(aad) == 0 || len(aad) > maxAADBytes {
		return nil, ErrKMSUnavailable
	}
	ciphertext := string(wrapped.Ciphertext)
	encryptionContext := string(aad)
	request := tencentkms.NewDecryptRequest()
	request.CiphertextBlob = &ciphertext
	request.EncryptionContext = &encryptionContext

	response, err := k.api.DecryptWithContext(ctx, request)
	if err != nil || response == nil || response.Response == nil || response.Response.Plaintext == nil || response.Response.KeyId == nil || *response.Response.KeyId != k.keyID {
		return nil, ErrKMSUnavailable
	}
	plaintext, err := base64.StdEncoding.DecodeString(*response.Response.Plaintext)
	if err != nil || len(plaintext) != dataKeySize {
		zeroBytes(plaintext)
		return nil, ErrKMSUnavailable
	}
	return plaintext, nil
}

func (c TencentKMSConfig) validate() error {
	if err := validateKeyMetadata(strings.TrimSpace(c.KeyID), 255); err != nil {
		return errors.New("Tencent KMS key id is required")
	}
	if err := validateKeyMetadata(strings.TrimSpace(c.KeyVersion), 128); err != nil {
		return errors.New("Tencent KMS key version label is required")
	}
	region := strings.TrimSpace(c.Region)
	if region == "" || len(region) > 64 || strings.ContainsAny(region, "/:@ ") {
		return errors.New("Tencent KMS region is invalid")
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 60 {
		return errors.New("Tencent KMS timeout must be between 1 and 60 seconds")
	}
	if endpoint := strings.TrimSpace(c.Endpoint); endpoint != "" {
		parsed, err := url.Parse("https://" + endpoint)
		if err != nil || parsed.Host != endpoint || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("Tencent KMS endpoint must be a host without scheme or path")
		}
	}
	return nil
}

func (c TencentKMSConfig) timeoutSeconds() int {
	if c.TimeoutSeconds == 0 {
		return defaultTencentKMSTimeoutSeconds
	}
	return c.TimeoutSeconds
}
