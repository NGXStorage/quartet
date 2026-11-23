package renderer

import (
	"testing"

	apps "k8s.io/api/apps/v1"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
)

// TestProfileWorkloadTargets pins Profile.WorkloadTargets() against the
// ACTUAL rendered operand set for every profile and platform — the
// reconciler's readiness probe (probeWorkloadReadiness) consumes these
// names to check each profile's OWN controller Deployment and node
// DaemonSet in its own operand namespace. Two failure modes must stay
// impossible (the adopted default readiness acceptance criterion; the NFS-name
// hardcode made FC/iSCSI unreachable for Ready):
//
// - a returned name that Objects() does not render (the probe would
// chase a workload that never exists for this profile);
// - two profiles sharing a workload name (a ready NFS-named workload
// planted in another profile's namespace would satisfy its readiness).
//
// For() dispatch is exercised here on purpose: the targets contract only
// matters for profiles the reconciler actually renders.
func TestProfileWorkloadTargets(t *testing.T) {
	wantProfiles := ProfileNames()
	seen := map[string]string{} // workload name -> owning profile
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
		for _, profileName := range wantProfiles {
			p := For(ProfileInput{
				Profile: profileName, SecretName: "s", DriverImage: "img",
				LogLevel: "info", Platform: platform,
			})
			if p == nil {
				t.Fatalf("profile %s platform %s: For returned nil — dispatch must reach every rendered profile", profileName, platform)
			}

			var renderedDep *apps.Deployment
			var renderedDaemonSet *apps.DaemonSet
			for _, obj := range p.Objects() {
				switch o := obj.(type) {
				case *apps.Deployment:
					renderedDep = o
				case *apps.DaemonSet:
					renderedDaemonSet = o
				}
			}
			if renderedDep == nil || renderedDaemonSet == nil {
				t.Fatalf("profile %s platform %s: Objects() must render exactly one controller Deployment and one node DaemonSet", profileName, platform)
			}

			depName, dsName := p.WorkloadTargets()
			if depName != renderedDep.Name {
				t.Errorf("profile %s platform %s: WorkloadTargets controller = %q, want rendered Deployment %q", profileName, platform, depName, renderedDep.Name)
			}
			if dsName != renderedDaemonSet.Name {
				t.Errorf("profile %s platform %s: WorkloadTargets node = %q, want rendered DaemonSet %q", profileName, platform, dsName, renderedDaemonSet.Name)
			}

			for _, name := range []string{depName, dsName} {
				if owner, ok := seen[name]; ok && owner != profileName {
					t.Errorf("workload name %q claimed by profile %s and %s — profiles must not share readiness targets", name, owner, profileName)
				}
				seen[name] = profileName
			}
		}
	}
}
