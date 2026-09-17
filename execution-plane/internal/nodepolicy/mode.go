// Package nodepolicy defines control-plane scheduling boundaries shared with
// host advertisements, without depending on either service's implementation.
package nodepolicy

const (
	ModeLabel               = "execution_mode"
	LifecycleOnlyMode       = "lifecycle-only"
	LifecycleOnlyCapability = "lifecycle_only"
)

// LifecycleOnly is a deny marker, not an optional placement requirement.
// Either marker excludes a node, including sticky and unconstrained requests.
func LifecycleOnly(labels map[string]string, capabilities []string) bool {
	if labels[ModeLabel] == LifecycleOnlyMode {
		return true
	}
	for _, value := range capabilities {
		if value == LifecycleOnlyCapability {
			return true
		}
	}
	return false
}
