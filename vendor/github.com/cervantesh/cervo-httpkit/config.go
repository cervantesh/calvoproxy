package cervohttpkit

import (
	"io"
	"net/http"
	"strings"
	"time"

	configenv "github.com/cervantesh/cervo-config"
)

// ClientConfig contains HTTP client and upstream target settings.
type ClientConfig struct {
	BaseURL string        `config:"CERVO_HTTP_BASE_URL" required:"true" desc:"Upstream service base URL"`
	Timeout time.Duration `config:"CERVO_HTTP_TIMEOUT" default:"5s" desc:"HTTP client timeout"`
	Token   string        `config:"CERVO_HTTP_TOKEN" desc:"Optional bearer token"`
}

// LoadClientConfig loads ClientConfig from a config loader.
func LoadClientConfig(loader *configenv.Loader) (ClientConfig, error) {
	if loader == nil {
		loader = configenv.New(configenv.Options{})
	}
	var cfg ClientConfig
	if err := loader.Decode(&cfg); err != nil {
		return ClientConfig{}, err
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	return cfg, nil
}

// NewClientFromConfig creates an HTTP client using cfg timeout.
func NewClientFromConfig(cfg ClientConfig) *http.Client {
	return DefaultJSONClient(cfg.Timeout)
}

// NewJSONRequestWithConfig builds a JSON request from typed client config.
func NewJSONRequestWithConfig(method string, cfg ClientConfig, path string, body io.Reader) (*http.Request, error) {
	req, err := NewJSONRequest(method, cfg.BaseURL, path, body)
	if err != nil {
		return nil, err
	}
	if token := strings.TrimSpace(cfg.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}
