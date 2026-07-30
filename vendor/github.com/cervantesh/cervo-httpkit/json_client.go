package cervohttpkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	configenv "github.com/cervantesh/cervo-config"
	"github.com/cervantesh/cervo-requestmeta"
)

func BaseURLFromEnv(envKey, fallback string) string {
	return strings.TrimRight(configenv.StringDefault(envKey, fallback), "/")
}

func DefaultJSONClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

func NewJSONRequest(method, baseURL, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, strings.TrimRight(baseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	requestmeta.ApplyTenantHeader(req, "")
	return req, nil
}

func DoJSON(client *http.Client, req *http.Request, dest interface{}) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if dest != nil {
		return json.NewDecoder(resp.Body).Decode(dest)
	}
	return nil
}

func PostJSON(client *http.Client, baseURL, path string, payload interface{}, dest interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := NewJSONRequest(http.MethodPost, baseURL, path, bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return DoJSON(client, req, dest)
}
