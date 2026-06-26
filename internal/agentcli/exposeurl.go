package agentcli

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"mitos.run/mitos/internal/preview"
)

// labelRE matches a valid single DNS label: starts and ends with alphanumeric,
// may contain hyphens in the middle, max 63 characters.
var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// BuildExposeURL validates label and exposeDomain, then returns the HTTPS URL
// for the expose endpoint: https://<label>.<exposeDomain>/. The label is
// lowercase-normalized before validation. Errors are returned for an empty or
// invalid label, a reserved label, or an empty exposeDomain.
func BuildExposeURL(label, exposeDomain string) (string, error) {
	label = strings.ToLower(label)

	if label == "" {
		return "", fmt.Errorf("expose URL: label is required")
	}
	if exposeDomain == "" {
		return "", fmt.Errorf("expose URL: expose domain is required")
	}
	if len(label) > 63 {
		return "", fmt.Errorf("expose URL: label %q exceeds 63 characters", label)
	}
	if !labelRE.MatchString(label) {
		return "", fmt.Errorf("expose URL: label %q is not a valid single DNS label (must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$)", label)
	}
	if preview.IsReservedLabel(label) {
		return "", fmt.Errorf("expose URL: label %q is reserved and may not be used by tenants", label)
	}
	return "https://" + label + "." + exposeDomain + "/", nil
}

// DefaultExposeDomain returns the value of the MITOS_EXPOSE_DOMAIN environment
// variable, or an empty string if it is not set.
func DefaultExposeDomain() string {
	return os.Getenv("MITOS_EXPOSE_DOMAIN")
}
