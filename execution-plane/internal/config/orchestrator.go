package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/go-sql-driver/mysql"
)

const (
	defaultOrchestratorRPCAddress = "127.0.0.1:8094"
	minimumOnboardingIntentTTL    = 5 * time.Minute
)

type OrchestratorRuntimeConfig struct {
	Enabled                       bool
	RPCListenAddress              string
	MySQLDSN                      string
	CACertificateFile             string
	CAPrivateKeyFile              string
	ServerCertificateFile         string
	ServerPrivateKeyFile          string
	ServerName                    string
	RotationRecipientEnvelopeFile string
	IntakeServiceID               string
	CertificateTTL                time.Duration
	IntentTTL                     time.Duration
	IntentClaimTTL                time.Duration
	ProvisioningPollInterval      time.Duration
	ProvisioningBatchSize         int
	KMS                           credential.TencentKMSConfig
}

func DefaultOrchestratorRuntimeConfig() OrchestratorRuntimeConfig {
	return OrchestratorRuntimeConfig{
		RPCListenAddress: defaultOrchestratorRPCAddress, IntakeServiceID: "ccmax",
		CertificateTTL: 24 * time.Hour, IntentTTL: 30 * time.Minute, IntentClaimTTL: 5 * time.Minute,
		ProvisioningPollInterval: time.Second, ProvisioningBatchSize: 100,
	}
}

func LoadOrchestratorRuntime(getenv func(string) string) (OrchestratorRuntimeConfig, error) {
	if getenv == nil {
		return OrchestratorRuntimeConfig{}, errors.New("orchestrator environment reader is required")
	}
	config := DefaultOrchestratorRuntimeConfig()
	enabled, err := parseStrictBool("EXECUTION_ORCHESTRATOR_RUNTIME_ENABLED", getenv("EXECUTION_ORCHESTRATOR_RUNTIME_ENABLED"))
	if err != nil {
		return OrchestratorRuntimeConfig{}, err
	}
	config.Enabled = enabled
	if !enabled {
		return config, nil
	}
	assignTrimmed(getenv, "EXECUTION_ORCHESTRATOR_RPC_LISTEN_ADDRESS", &config.RPCListenAddress)
	config.MySQLDSN = strings.TrimSpace(getenv("EXECUTION_MYSQL_DSN"))
	config.CACertificateFile = strings.TrimSpace(getenv("EXECUTION_CA_CERT_FILE"))
	config.CAPrivateKeyFile = strings.TrimSpace(getenv("EXECUTION_CA_KEY_FILE"))
	config.ServerCertificateFile = strings.TrimSpace(getenv("EXECUTION_SERVER_CERT_FILE"))
	config.ServerPrivateKeyFile = strings.TrimSpace(getenv("EXECUTION_SERVER_KEY_FILE"))
	config.ServerName = strings.TrimSpace(getenv("EXECUTION_SERVER_NAME"))
	config.RotationRecipientEnvelopeFile = strings.TrimSpace(getenv("EXECUTION_ROTATION_RECIPIENT_ENVELOPE_FILE"))
	assignTrimmed(getenv, "EXECUTION_ONBOARDING_INTAKE_SERVICE_ID", &config.IntakeServiceID)
	for _, field := range []struct {
		name   string
		target *time.Duration
	}{
		{"EXECUTION_CERTIFICATE_TTL", &config.CertificateTTL},
		{"EXECUTION_ONBOARDING_INTENT_TTL", &config.IntentTTL},
		{"EXECUTION_ONBOARDING_CLAIM_TTL", &config.IntentClaimTTL},
		{"EXECUTION_ONBOARDING_POLL_INTERVAL", &config.ProvisioningPollInterval},
	} {
		if err := setDuration(getenv, field.name, field.target); err != nil {
			return OrchestratorRuntimeConfig{}, err
		}
	}
	if err := setPositiveInt(getenv, "EXECUTION_ONBOARDING_BATCH_SIZE", &config.ProvisioningBatchSize); err != nil {
		return OrchestratorRuntimeConfig{}, err
	}
	config.KMS = credential.TencentKMSConfig{
		Region:      strings.TrimSpace(getenv("EXECUTION_KMS_REGION")),
		KeyID:       strings.TrimSpace(getenv("EXECUTION_KMS_KEY_ID")),
		KeyVersion:  strings.TrimSpace(getenv("EXECUTION_KMS_KEY_VERSION")),
		Endpoint:    strings.TrimSpace(getenv("EXECUTION_KMS_ENDPOINT")),
		CVMRoleName: strings.TrimSpace(getenv("EXECUTION_KMS_CVM_ROLE_NAME")),
	}
	if value := strings.TrimSpace(getenv("EXECUTION_KMS_TIMEOUT_SECONDS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return OrchestratorRuntimeConfig{}, fmt.Errorf("EXECUTION_KMS_TIMEOUT_SECONDS: %w", err)
		}
		config.KMS.TimeoutSeconds = parsed
	}
	if err := config.Validate(); err != nil {
		return OrchestratorRuntimeConfig{}, err
	}
	return config, nil
}

func (c OrchestratorRuntimeConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if err := validateListenAddress(c.RPCListenAddress); err != nil {
		return fmt.Errorf("orchestrator RPC address: %w", err)
	}
	if err := validateRuntimeMySQLDSN(c.MySQLDSN); err != nil {
		return err
	}
	paths := []string{
		c.CACertificateFile, c.CAPrivateKeyFile, c.ServerCertificateFile,
		c.ServerPrivateKeyFile, c.RotationRecipientEnvelopeFile,
	}
	seenPaths := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("orchestrator certificate and envelope paths must be clean absolute paths")
		}
		if _, exists := seenPaths[path]; exists {
			return errors.New("orchestrator certificate and envelope paths must be distinct")
		}
		seenPaths[path] = struct{}{}
	}
	if pki.ValidateServiceID(c.IntakeServiceID) != nil {
		return errors.New("orchestrator intake service id is invalid")
	}
	if c.ServerName == "" || len(c.ServerName) > 253 || strings.ContainsAny(c.ServerName, "/:@ ") {
		return errors.New("orchestrator server name is invalid")
	}
	// CCMAX may spend up to two minutes on one bounded intake RPC and requires
	// an additional commit margin before accepting the receipt. Keep a wider
	// production floor so a valid configuration cannot make every onboarding
	// attempt expire before its durable account/outbox transaction commits.
	if c.CertificateTTL <= 0 || c.CertificateTTL > 7*24*time.Hour ||
		c.IntentTTL < minimumOnboardingIntentTTL || c.IntentTTL > 24*time.Hour ||
		c.IntentClaimTTL <= 0 || c.IntentClaimTTL > c.IntentTTL || c.ProvisioningPollInterval <= 0 ||
		c.ProvisioningPollInterval > time.Minute || c.ProvisioningBatchSize < 1 || c.ProvisioningBatchSize > 1000 {
		return errors.New("orchestrator runtime timing or batch configuration is invalid")
	}
	if !validRuntimeIdentifier(c.KMS.CVMRoleName, 128) {
		return errors.New("Tencent KMS CVM role name is required")
	}
	if err := c.KMS.Validate(); err != nil {
		return fmt.Errorf("orchestrator KMS configuration: %w", err)
	}
	return nil
}

func (c OrchestratorRuntimeConfig) String() string {
	return fmt.Sprintf("OrchestratorRuntimeConfig{Enabled:%t RPCListenAddress:%q MySQLDSN:[REDACTED] CACertificateFile:%q CAPrivateKeyFile:%q ServerCertificateFile:%q ServerPrivateKeyFile:%q ServerName:%q RotationRecipientEnvelopeFile:%q IntakeServiceID:%q CertificateTTL:%s IntentTTL:%s IntentClaimTTL:%s ProvisioningPollInterval:%s ProvisioningBatchSize:%d KMS:{Region:%q KeyID:%q KeyVersion:%q Endpoint:%q CVMRoleName:%q TimeoutSeconds:%d}}",
		c.Enabled, c.RPCListenAddress, c.CACertificateFile, c.CAPrivateKeyFile, c.ServerCertificateFile,
		c.ServerPrivateKeyFile, c.ServerName, c.RotationRecipientEnvelopeFile, c.IntakeServiceID, c.CertificateTTL,
		c.IntentTTL, c.IntentClaimTTL, c.ProvisioningPollInterval, c.ProvisioningBatchSize,
		c.KMS.Region, c.KMS.KeyID, c.KMS.KeyVersion, c.KMS.Endpoint, c.KMS.CVMRoleName, c.KMS.TimeoutSeconds)
}

func (c OrchestratorRuntimeConfig) GoString() string { return c.String() }

func (c OrchestratorRuntimeConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Enabled               bool   `json:"enabled"`
		RPCListenAddress      string `json:"rpc_listen_address"`
		MySQLDSNConfigured    bool   `json:"mysql_dsn_configured"`
		IntakeServiceID       string `json:"intake_service_id"`
		ProvisioningBatchSize int    `json:"provisioning_batch_size"`
	}{c.Enabled, c.RPCListenAddress, c.MySQLDSN != "", c.IntakeServiceID, c.ProvisioningBatchSize})
}

func parseStrictBool(name, value string) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "false") {
		return false, nil
	}
	if strings.EqualFold(value, "true") {
		return true, nil
	}
	return false, fmt.Errorf("%s must be true or false", name)
}

func assignTrimmed(getenv func(string) string, name string, target *string) {
	if value := strings.TrimSpace(getenv(name)); value != "" {
		*target = value
	}
}

func validateRuntimeMySQLDSN(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("EXECUTION_MYSQL_DSN is required")
	}
	parsed, err := mysql.ParseDSN(value)
	if err != nil || parsed.User == "" || parsed.Net != "tcp" && parsed.Net != "tcp4" && parsed.Net != "tcp6" ||
		parsed.Addr == "" || parsed.DBName == "" || !parsed.ParseTime || parsed.Loc == nil || parsed.Loc.String() != "UTC" {
		return errors.New("EXECUTION_MYSQL_DSN must use TCP, a database, parseTime=true and loc=UTC")
	}
	tlsMode := strings.ToLower(strings.TrimSpace(parsed.TLSConfig))
	if tlsMode == "" || tlsMode == "false" || tlsMode == "skip-verify" || tlsMode == "preferred" {
		return errors.New("EXECUTION_MYSQL_DSN must require verified TLS")
	}
	return nil
}

func validRuntimeIdentifier(value string, maxBytes int) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxBytes {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}
