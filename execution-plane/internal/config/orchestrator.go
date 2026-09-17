package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/go-sql-driver/mysql"
)

const (
	defaultOrchestratorRPCAddress       = "127.0.0.1:8094"
	defaultRuntimeOutboxConsumerName    = "sub2api-execution-runtime-v1"
	defaultRuntimeOutboxLeaseTTL        = 30 * time.Second
	defaultRuntimeOutboxPollInterval    = time.Second
	defaultRuntimeOutboxBatchSize       = 200
	defaultRuntimeOutboxMaxFailures     = 5
	defaultWorkerCPURequestMillis       = 500
	defaultWorkerMemoryRequestBytes     = 128 << 20
	defaultStartObservationMaxAge       = 45 * time.Second
	defaultStartCommandTTL              = 2 * time.Minute
	defaultStartCoordinatorPollInterval = time.Second
	defaultStartCoordinatorBatchSize    = 200
	defaultStartCoordinatorClaimTTL     = 30 * time.Second
	defaultStartCoordinatorRetryDelay   = 2 * time.Second
	defaultRoutePublishTTL              = 45 * time.Second
	defaultRoutePublishInterval         = 5 * time.Second
	minimumOnboardingIntentTTL          = 5 * time.Minute
)

var workerImageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type OrchestratorRuntimeConfig struct {
	Enabled                          bool
	RPCListenAddress                 string
	MySQLDSN                         string
	CCMAXMySQLDSN                    string
	CoordinatorInstanceID            string
	RuntimeOutboxConsumerName        string
	RuntimeOutboxLeaseTTL            time.Duration
	RuntimeOutboxPollInterval        time.Duration
	RuntimeOutboxBatchSize           int
	RuntimeOutboxMaxRetryFailures    int
	WorkerImageDigest                string
	WorkerRequiredLabels             map[string]string
	WorkerCPURequestMillis           uint64
	WorkerMemoryRequestBytes         uint64
	CACertificateFile                string
	CAPrivateKeyFile                 string
	ServerCertificateFile            string
	ServerPrivateKeyFile             string
	ServerName                       string
	RotationRecipientEnvelopeFile    string
	IntakeServiceID                  string
	CertificateTTL                   time.Duration
	IntentTTL                        time.Duration
	IntentClaimTTL                   time.Duration
	ProvisioningPollInterval         time.Duration
	ProvisioningBatchSize            int
	OnboardingStartObservationMaxAge time.Duration
	OnboardingStartCommandTTL        time.Duration
	OnboardingStartPollInterval      time.Duration
	OnboardingStartBatchSize         int
	OnboardingStartClaimTTL          time.Duration
	OnboardingStartRetryDelay        time.Duration
	RouteRedisAddr                   string
	RoutePublishTTL                  time.Duration
	RoutePublishInterval             time.Duration
	RuntimeEnrollment                RuntimeEnrollmentConfig
	KMS                              credential.TencentKMSConfig
}

func DefaultOrchestratorRuntimeConfig() OrchestratorRuntimeConfig {
	return OrchestratorRuntimeConfig{
		RPCListenAddress: defaultOrchestratorRPCAddress, IntakeServiceID: "ccmax",
		CertificateTTL: 24 * time.Hour, IntentTTL: 30 * time.Minute, IntentClaimTTL: 5 * time.Minute,
		ProvisioningPollInterval: time.Second, ProvisioningBatchSize: 100,
		RuntimeOutboxConsumerName: defaultRuntimeOutboxConsumerName,
		RuntimeOutboxLeaseTTL:     defaultRuntimeOutboxLeaseTTL, RuntimeOutboxPollInterval: defaultRuntimeOutboxPollInterval,
		RuntimeOutboxBatchSize: defaultRuntimeOutboxBatchSize, RuntimeOutboxMaxRetryFailures: defaultRuntimeOutboxMaxFailures,
		WorkerRequiredLabels: make(map[string]string), WorkerCPURequestMillis: defaultWorkerCPURequestMillis,
		WorkerMemoryRequestBytes:         defaultWorkerMemoryRequestBytes,
		OnboardingStartObservationMaxAge: defaultStartObservationMaxAge,
		OnboardingStartCommandTTL:        defaultStartCommandTTL, OnboardingStartPollInterval: defaultStartCoordinatorPollInterval,
		OnboardingStartBatchSize: defaultStartCoordinatorBatchSize, OnboardingStartClaimTTL: defaultStartCoordinatorClaimTTL,
		OnboardingStartRetryDelay: defaultStartCoordinatorRetryDelay,
		RoutePublishTTL:           defaultRoutePublishTTL, RoutePublishInterval: defaultRoutePublishInterval,
		RuntimeEnrollment: DefaultRuntimeEnrollmentConfig(),
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
	config.RuntimeEnrollment, err = LoadRuntimeEnrollment(getenv)
	if err != nil {
		return OrchestratorRuntimeConfig{}, err
	}
	assignTrimmed(getenv, "EXECUTION_ORCHESTRATOR_RPC_LISTEN_ADDRESS", &config.RPCListenAddress)
	config.MySQLDSN = strings.TrimSpace(getenv("EXECUTION_MYSQL_DSN"))
	config.CCMAXMySQLDSN = strings.TrimSpace(getenv("EXECUTION_CCMAX_MYSQL_DSN"))
	config.CoordinatorInstanceID = strings.TrimSpace(getenv("EXECUTION_COORDINATOR_INSTANCE_ID"))
	assignTrimmed(getenv, "EXECUTION_RUNTIME_OUTBOX_CONSUMER_NAME", &config.RuntimeOutboxConsumerName)
	config.WorkerImageDigest = strings.TrimSpace(getenv("EXECUTION_WORKER_IMAGE_DIGEST"))
	if labels := strings.TrimSpace(getenv("EXECUTION_WORKER_REQUIRED_LABELS_JSON")); labels != "" {
		if err := json.Unmarshal([]byte(labels), &config.WorkerRequiredLabels); err != nil || config.WorkerRequiredLabels == nil {
			return OrchestratorRuntimeConfig{}, errors.New("EXECUTION_WORKER_REQUIRED_LABELS_JSON must be a JSON object of strings")
		}
	}
	config.CACertificateFile = strings.TrimSpace(getenv("EXECUTION_CA_CERT_FILE"))
	config.CAPrivateKeyFile = strings.TrimSpace(getenv("EXECUTION_CA_KEY_FILE"))
	config.ServerCertificateFile = strings.TrimSpace(getenv("EXECUTION_SERVER_CERT_FILE"))
	config.ServerPrivateKeyFile = strings.TrimSpace(getenv("EXECUTION_SERVER_KEY_FILE"))
	config.ServerName = strings.TrimSpace(getenv("EXECUTION_SERVER_NAME"))
	config.RotationRecipientEnvelopeFile = strings.TrimSpace(getenv("EXECUTION_ROTATION_RECIPIENT_ENVELOPE_FILE"))
	assignTrimmed(getenv, "EXECUTION_ONBOARDING_INTAKE_SERVICE_ID", &config.IntakeServiceID)
	assignTrimmed(getenv, "EXECUTION_ROUTE_REDIS_ADDR", &config.RouteRedisAddr)
	for _, field := range []struct {
		name   string
		target *time.Duration
	}{
		{"EXECUTION_CERTIFICATE_TTL", &config.CertificateTTL},
		{"EXECUTION_ONBOARDING_INTENT_TTL", &config.IntentTTL},
		{"EXECUTION_ONBOARDING_CLAIM_TTL", &config.IntentClaimTTL},
		{"EXECUTION_ONBOARDING_POLL_INTERVAL", &config.ProvisioningPollInterval},
		{"EXECUTION_RUNTIME_OUTBOX_LEASE_TTL", &config.RuntimeOutboxLeaseTTL},
		{"EXECUTION_RUNTIME_OUTBOX_POLL_INTERVAL", &config.RuntimeOutboxPollInterval},
		{"EXECUTION_ONBOARDING_START_OBSERVATION_MAX_AGE", &config.OnboardingStartObservationMaxAge},
		{"EXECUTION_ONBOARDING_START_COMMAND_TTL", &config.OnboardingStartCommandTTL},
		{"EXECUTION_ONBOARDING_START_POLL_INTERVAL", &config.OnboardingStartPollInterval},
		{"EXECUTION_ONBOARDING_START_CLAIM_TTL", &config.OnboardingStartClaimTTL},
		{"EXECUTION_ONBOARDING_START_RETRY_DELAY", &config.OnboardingStartRetryDelay},
		{"EXECUTION_ROUTE_PUBLISH_TTL", &config.RoutePublishTTL},
		{"EXECUTION_ROUTE_PUBLISH_INTERVAL", &config.RoutePublishInterval},
	} {
		if err := setDuration(getenv, field.name, field.target); err != nil {
			return OrchestratorRuntimeConfig{}, err
		}
	}
	if err := setPositiveInt(getenv, "EXECUTION_ONBOARDING_BATCH_SIZE", &config.ProvisioningBatchSize); err != nil {
		return OrchestratorRuntimeConfig{}, err
	}
	if err := setPositiveInt(getenv, "EXECUTION_RUNTIME_OUTBOX_BATCH_SIZE", &config.RuntimeOutboxBatchSize); err != nil {
		return OrchestratorRuntimeConfig{}, err
	}
	if err := setPositiveInt(getenv, "EXECUTION_RUNTIME_OUTBOX_MAX_RETRY_FAILURES", &config.RuntimeOutboxMaxRetryFailures); err != nil {
		return OrchestratorRuntimeConfig{}, err
	}
	if err := setPositiveInt(getenv, "EXECUTION_ONBOARDING_START_BATCH_SIZE", &config.OnboardingStartBatchSize); err != nil {
		return OrchestratorRuntimeConfig{}, err
	}
	if err := setPositiveUint64(getenv, "EXECUTION_WORKER_CPU_REQUEST_MILLIS", &config.WorkerCPURequestMillis); err != nil {
		return OrchestratorRuntimeConfig{}, err
	}
	if err := setPositiveUint64(getenv, "EXECUTION_WORKER_MEMORY_REQUEST_BYTES", &config.WorkerMemoryRequestBytes); err != nil {
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
	if err := c.RuntimeEnrollment.Validate(); err != nil {
		return err
	}
	if err := validateListenAddress(c.RPCListenAddress); err != nil {
		return fmt.Errorf("orchestrator RPC address: %w", err)
	}
	if err := validateRuntimeMySQLDSN("EXECUTION_MYSQL_DSN", c.MySQLDSN); err != nil {
		return err
	}
	if err := validateRuntimeMySQLDSN("EXECUTION_CCMAX_MYSQL_DSN", c.CCMAXMySQLDSN); err != nil {
		return err
	}
	if runtimeDatabase, _ := runtimeMySQLDatabaseName(c.MySQLDSN); runtimeDatabase == "" {
		return errors.New("EXECUTION_MYSQL_DSN database is invalid")
	} else if ccmaxDatabase, _ := runtimeMySQLDatabaseName(c.CCMAXMySQLDSN); ccmaxDatabase == "" || ccmaxDatabase == runtimeDatabase {
		return errors.New("CCMAX and worker runtime MySQL databases must be distinct")
	}
	if !validRuntimeIdentifier(c.CoordinatorInstanceID, 128) || !validRuntimeIdentifier(c.RuntimeOutboxConsumerName, 128) {
		return errors.New("orchestrator coordinator and outbox identities are invalid")
	}
	if !workerImageDigestPattern.MatchString(c.WorkerImageDigest) || len(c.WorkerRequiredLabels) > 32 ||
		c.WorkerCPURequestMillis == 0 || c.WorkerCPURequestMillis > 64_000 ||
		c.WorkerMemoryRequestBytes < 16<<20 || c.WorkerMemoryRequestBytes > 16<<30 {
		return errors.New("worker image or resource policy is invalid")
	}
	for key, value := range c.WorkerRequiredLabels {
		if strings.TrimSpace(key) == "" || key != strings.TrimSpace(key) || len(key) > 64 || len(value) > 128 {
			return errors.New("worker required labels are invalid")
		}
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
		c.ProvisioningPollInterval > time.Minute || c.ProvisioningBatchSize < 1 || c.ProvisioningBatchSize > 1000 ||
		c.RuntimeOutboxLeaseTTL <= 0 || c.RuntimeOutboxLeaseTTL > time.Hour ||
		c.RuntimeOutboxPollInterval <= 0 || c.RuntimeOutboxPollInterval > time.Minute ||
		c.RuntimeOutboxBatchSize < 1 || c.RuntimeOutboxBatchSize > 1000 ||
		c.RuntimeOutboxMaxRetryFailures < 1 || c.RuntimeOutboxMaxRetryFailures > 1000 ||
		c.OnboardingStartObservationMaxAge <= 0 || c.OnboardingStartObservationMaxAge > time.Hour ||
		c.OnboardingStartCommandTTL <= 0 || c.OnboardingStartCommandTTL > 24*time.Hour ||
		c.OnboardingStartPollInterval <= 0 || c.OnboardingStartPollInterval > time.Minute ||
		c.OnboardingStartBatchSize < 1 || c.OnboardingStartBatchSize > 1000 ||
		c.OnboardingStartClaimTTL <= 0 || c.OnboardingStartClaimTTL > time.Hour ||
		c.OnboardingStartRetryDelay <= 0 || c.OnboardingStartRetryDelay > time.Hour ||
		c.RoutePublishTTL <= 0 || c.RoutePublishTTL > time.Minute ||
		c.RoutePublishInterval <= 0 || c.RoutePublishInterval > time.Minute {
		return errors.New("orchestrator runtime timing or batch configuration is invalid")
	}
	if err := validateListenAddress(c.RouteRedisAddr); err != nil {
		return errors.New("orchestrator route Redis address is invalid")
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
	return fmt.Sprintf("OrchestratorRuntimeConfig{Enabled:%t RPCListenAddress:%q MySQLDSN:[REDACTED] CCMAXMySQLDSN:[REDACTED] CoordinatorInstanceID:%q RuntimeOutboxConsumerName:%q WorkerImageDigest:%q CACertificateFile:%q CAPrivateKeyFile:%q ServerCertificateFile:%q ServerPrivateKeyFile:%q ServerName:%q RotationRecipientEnvelopeFile:%q IntakeServiceID:%q CertificateTTL:%s IntentTTL:%s IntentClaimTTL:%s ProvisioningPollInterval:%s ProvisioningBatchSize:%d KMS:{Region:%q KeyID:%q KeyVersion:%q Endpoint:%q CVMRoleName:%q TimeoutSeconds:%d}}",
		c.Enabled, c.RPCListenAddress, c.CoordinatorInstanceID, c.RuntimeOutboxConsumerName, c.WorkerImageDigest,
		c.CACertificateFile, c.CAPrivateKeyFile, c.ServerCertificateFile,
		c.ServerPrivateKeyFile, c.ServerName, c.RotationRecipientEnvelopeFile, c.IntakeServiceID, c.CertificateTTL,
		c.IntentTTL, c.IntentClaimTTL, c.ProvisioningPollInterval, c.ProvisioningBatchSize,
		c.KMS.Region, c.KMS.KeyID, c.KMS.KeyVersion, c.KMS.Endpoint, c.KMS.CVMRoleName, c.KMS.TimeoutSeconds)
}

func (c OrchestratorRuntimeConfig) GoString() string { return c.String() }

func (c OrchestratorRuntimeConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Enabled                 bool   `json:"enabled"`
		RPCListenAddress        string `json:"rpc_listen_address"`
		MySQLDSNConfigured      bool   `json:"mysql_dsn_configured"`
		CCMAXMySQLDSNConfigured bool   `json:"ccmax_mysql_dsn_configured"`
		IntakeServiceID         string `json:"intake_service_id"`
		ProvisioningBatchSize   int    `json:"provisioning_batch_size"`
	}{c.Enabled, c.RPCListenAddress, c.MySQLDSN != "", c.CCMAXMySQLDSN != "", c.IntakeServiceID, c.ProvisioningBatchSize})
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

func validateRuntimeMySQLDSN(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	parsed, err := mysql.ParseDSN(value)
	if err != nil || parsed.User == "" || parsed.Net != "tcp" && parsed.Net != "tcp4" && parsed.Net != "tcp6" ||
		parsed.Addr == "" || parsed.DBName == "" || !parsed.ParseTime || parsed.Loc == nil || parsed.Loc.String() != "UTC" {
		return fmt.Errorf("%s must use TCP, a database, parseTime=true and loc=UTC", name)
	}
	tlsMode := strings.ToLower(strings.TrimSpace(parsed.TLSConfig))
	if tlsMode == "" || tlsMode == "false" || tlsMode == "skip-verify" || tlsMode == "preferred" {
		return fmt.Errorf("%s must require verified TLS", name)
	}
	return nil
}

func runtimeMySQLDatabaseName(value string) (string, error) {
	parsed, err := mysql.ParseDSN(value)
	if err != nil {
		return "", err
	}
	return parsed.DBName, nil
}

func setPositiveUint64(getenv func(string) string, name string, target *uint64) error {
	if value := strings.TrimSpace(getenv(name)); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil || parsed == 0 {
			return fmt.Errorf("%s must be a positive integer", name)
		}
		*target = parsed
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
