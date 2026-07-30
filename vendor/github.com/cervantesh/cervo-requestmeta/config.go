package requestmeta

import (
	"net/http"
	"strings"

	configenv "github.com/cervantesh/cervo-config"
	"github.com/cervantesh/cervo-config/calvoproxy"
)

// Config contains request metadata defaults loaded from configuration sources.
type Config struct {
	TenantID      string `config:"CERVOCLAW_TENANT_ID" alias:"OPENCLAW_TENANT_ID" default:"default" desc:"Tenant identifier propagated to downstream services"`
	ForwardAuth   bool   `config:"CERVO_FORWARD_AUTH" default:"true" desc:"Whether default bearer auth should be applied"`
	DefaultBearer string `config:"CERVO_DEFAULT_BEARER" desc:"Optional bearer token for downstream service calls"`
}

// LoadConfig loads request metadata defaults from a config loader.
func LoadConfig(loader *configenv.Loader) (Config, error) {
	if loader == nil {
		loader = calvoproxy.NewLoader()
	}
	var cfg Config
	if err := loader.Decode(&cfg); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(cfg.TenantID) == "" {
		cfg.TenantID = "default"
	}
	return cfg, nil
}

// ApplyHeaders applies configured tenant and auth metadata to req.
func ApplyHeaders(req *http.Request, cfg Config) {
	if req == nil {
		return
	}
	ApplyTenantHeader(req, cfg.TenantID)
	if !cfg.ForwardAuth || strings.TrimSpace(cfg.DefaultBearer) == "" || req.Header.Get(HeaderAuthorization) != "" {
		return
	}
	req.Header.Set(HeaderAuthorization, "Bearer "+strings.TrimSpace(cfg.DefaultBearer))
}
