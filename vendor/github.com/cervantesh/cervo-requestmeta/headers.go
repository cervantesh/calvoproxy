package requestmeta

import (
	"net/http"
	"strings"

	"github.com/cervantesh/cervo-config/calvoproxy"
)

const (
	HeaderTenantID      = "X-Tenant-ID"
	HeaderAuthorization = "Authorization"
)

func TenantIDFromEnv() string {
	tenantID := strings.TrimSpace(calvoproxy.String("CERVOCLAW_TENANT_ID"))
	if tenantID == "" {
		return "default"
	}
	return tenantID
}

func ApplyTenantHeader(req *http.Request, tenantID string) {
	if req == nil {
		return
	}
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		tenantID = TenantIDFromEnv()
	}
	req.Header.Set(HeaderTenantID, tenantID)
}

func BearerToken(headerValue string) string {
	value := strings.TrimSpace(headerValue)
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return value
}

func AuthorizationFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	return BearerToken(r.Header.Get(HeaderAuthorization))
}
