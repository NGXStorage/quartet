package controller

import (
	"errors"
	"fmt"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// openShiftConfigGroup is the API group whose presence marks an OpenShift
// platform (discovery contract).
const openShiftConfigGroup = "config.openshift.io"

// platformDiscovery is the minimal discovery surface the operator needs.
// k8s.io/client-go discovery.DiscoveryInterface satisfies it; tests use a
// fake. (envtest cannot simulate an OpenShift API group, so the OCP branch
// is covered by unit tests with the fake client.)
type platformDiscovery interface {
	ServerGroups() (*metav1.APIGroupList, error)
}

// Sentinel detection errors. They map 1:1 onto condition reasons: discovery
// permission failures and generic discovery/network failures are DISTINCT
// outcomes (API absence must be distinguished from permission/network
// error).
var (
	// ErrPlatformConflict: explicit spec.platform override disagrees with
	// what discovery reports. → reason PlatformDetectionFailed; silent
	// fallback is forbidden.
	ErrPlatformConflict = errors.New("spec.platform override conflicts with API discovery")
	// ErrDiscoveryPermission: the API server denied discovery. → reason
	// PermissionDenied.
	ErrDiscoveryPermission = errors.New("API discovery denied by the server")
	// ErrDiscoveryFailure: network/API-server error that is not an
	// authorization decision. → reason DependencyMissing.
	ErrDiscoveryFailure = errors.New("API discovery failed")
	// ErrUnsupportedPlatform: declared value outside the enum (defense in
	// depth; schema + ValidateSpec make this unreachable in practice). →
	// reason UnsupportedPlatform.
	ErrUnsupportedPlatform = errors.New("unsupported platform value")
)

// DetectPlatform resolves the effective platform for a declared
// spec.platform value. It never falls back silently: a conflict
// between an explicit override and discovery is an error.
func DetectPlatform(d platformDiscovery, declared v1alpha1.Platform) (v1alpha1.Platform, error) {
	switch declared {
	case v1alpha1.PlatformAuto, v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift:
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedPlatform, declared)
	}

	groups, err := d.ServerGroups()
	if err != nil {
		if apierrors.IsForbidden(err) {
			return "", fmt.Errorf("%w: %v", ErrDiscoveryPermission, err)
		}
		return "", fmt.Errorf("%w: %v", ErrDiscoveryFailure, err)
	}

	hasOpenShift := false
	for _, g := range groups.Groups {
		if g.Name == openShiftConfigGroup {
			hasOpenShift = true
			break
		}
	}

	detected := v1alpha1.PlatformKubernetes
	if hasOpenShift {
		detected = v1alpha1.PlatformOpenShift
	}

	if declared == v1alpha1.PlatformAuto {
		return detected, nil
	}
	if declared != detected {
		return "", fmt.Errorf("%w: spec.platform=%q but discovery reports %q", ErrPlatformConflict, declared, detected)
	}
	return declared, nil
}
