package controller

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-logr/zapr"
	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	rbacPlaneOnce sync.Once
	rbacPlaneErr  error
	// stopRBACPlaneManager shuts the token-only manager down before the
	// API server (wired into stopEnvtest).
	stopRBACPlaneManager func()
	// rbacPlaneLogs captures the plane reconciler's logs so a failed poll
	// can dump what the manager actually saw.
	rbacPlaneLogs *observer.ObservedLogs
)

// startRBACPlaneManager boots the real Quartet manager over the token-only
// SA rest config and waits for cache sync. The sync is part of the proof:
// the manager lists/watches every watched kind through the RBAC layer —
// including the four install-owned RBAC kinds whose grant must
// come from the generated role.yaml, and it fails fast (30s) rather than
// spinning on a denied informer.
func startRBACPlaneManager(t *testing.T) {
	t.Helper()
	startRBACAPI(t)
	rbacPlaneOnce.Do(func() {
		core, obs := observer.New(zapcore.DebugLevel)
		rbacPlaneLogs = obs
		planeLogger := zapr.NewLogger(zap.New(core))
		// Route ctrl.Log (internal-controller error logging) into the same
		// sink — an unlogged reconcile error would otherwise be invisible.
		logf.SetLogger(planeLogger)
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			rbacPlaneErr = err
			return
		}
		if err := v1alpha1.AddToScheme(scheme); err != nil {
			rbacPlaneErr = err
			return
		}
		direct, err := client.New(rbacSACfg, client.Options{Scheme: scheme})
		if err != nil {
			rbacPlaneErr = err
			return
		}
		saDiscovery, err := discovery.NewDiscoveryClientForConfig(rbacSACfg)
		if err != nil {
			rbacPlaneErr = err
			return
		}
		mgr, err := ctrl.NewManager(rbacSACfg, ctrl.Options{
			Scheme:                 scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			LeaderElection:         false,
			// controller-runtime names controllers globally for metrics;
			// the main-plane suite already owns the name "quartet".
			Controller: config.Controller{SkipNameValidation: ptr.To(true)},
		})
		if err != nil {
			rbacPlaneErr = err
			return
		}
		if err := (&QuartetReconciler{
			Client:            mgr.GetClient(),
			DirectClient:      direct,
			Scheme:            mgr.GetScheme(),
			Log:               planeLogger.WithName("Quartet"),
			PlatformDiscovery: saDiscovery,
		}).SetupWithManager(mgr); err != nil {
			rbacPlaneErr = err
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- mgr.Start(ctx) }()
		syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
		defer syncCancel()
		if !mgr.GetCache().WaitForCacheSync(syncCtx) {
			cancel()
			<-done
			rbacPlaneErr = errors.New("token-only manager cache did not sync — a watch is not authorized by the generated role.yaml")
			return
		}
		stopRBACPlaneManager = func() {
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Logf("rbac plane manager shutdown: %v", err)
				}
			case <-time.After(suiteTimeout):
			}
		}
	})
	if rbacPlaneErr != nil {
		t.Fatalf("token-only plane manager unavailable: %v", rbacPlaneErr)
	}
}

// pollStatusOn is the client-parameterized status poller (the plane
// helpers mirror the main-suite ones without touching its globals).
func pollStatusOn(t *testing.T, c client.Reader, name string, check func(*v1alpha1.Quartet) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, suiteTimeout, true, func(ctx context.Context) (bool, error) {
		q := &v1alpha1.Quartet{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, q); err != nil {
			return false, nil // transient get failure: keep polling
		}
		return check(q), nil
	})
	if err != nil {
		t.Fatalf("condition for %q not reached within %s: %v\nplane reconciler logs:\n%s",
			name, suiteTimeout, err, planeLogTail(40))
	}
}

// planeLogTail renders the last n captured plane reconciler log entries —
// a failed poll dumps them so a stuck reconcile is self-explaining.
func planeLogTail(n int) string {
	if rbacPlaneLogs == nil {
		return "(no logs captured)"
	}
	entries := rbacPlaneLogs.All()
	if len(entries) == 0 {
		return "(plane reconciler produced no logs)"
	}
	start := len(entries) - n
	if start < 0 {
		start = 0
	}
	var b strings.Builder
	for _, e := range entries[start:] {
		fmt.Fprintf(&b, "[%s] %s %v\n", e.Level, e.Message, e.ContextMap())
	}
	return b.String()
}

func createSingletonOn(t *testing.T, c client.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()
	if err := c.Create(ctx, validCR()); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create singleton on rbac plane: %v", err)
	}
}

// resetSingletonOn deletes any existing singleton and waits for it to be
// fully reaped. The plane manager (already running) performs the finalizer
// cleanup — this guarantees a test starts from NO prior CR state instead of
// inheriting the previous test's Ready singleton (the install-set
// lifecycle test leaves one behind).
func resetSingletonOn(t *testing.T, c client.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*suiteTimeout)
	defer cancel()
	existing := &v1alpha1.Quartet{}
	if err := c.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, existing); err != nil {
		return // already absent (or NotFound) — nothing to reset
	}
	if err := c.Delete(ctx, existing); err != nil {
		t.Fatalf("delete prior singleton: %v", err)
	}
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 2*suiteTimeout, true, func(ctx context.Context) (bool, error) {
		q := &v1alpha1.Quartet{}
		if err := c.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
			return client.IgnoreNotFound(err) == nil, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("prior singleton not reaped within timeout: %v", err)
	}
}

// convergeWorkloadsOn simulates the kubelet/Deployment-controller/DaemonSet-
// controller convergence the main suite's convergeWorkloads does.
func convergeWorkloadsOn(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	d := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller", Namespace: "ngxstorage-nfs"}, d); err != nil {
		t.Fatalf("Deployment missing for status simulation: %v", err)
	}
	d.Status.ObservedGeneration = d.Generation
	d.Status.Replicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas, d.Status.ReadyReplicas = 1, 1, 1, 1
	if err := c.Status().Update(ctx, d); err != nil {
		t.Fatalf("set Deployment status: %v", err)
	}
	ds := &appsv1.DaemonSet{}
	if err := c.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-node", Namespace: "ngxstorage-nfs"}, ds); err != nil {
		t.Fatalf("DaemonSet missing for status simulation: %v", err)
	}
	ds.Status.ObservedGeneration = ds.Generation
	ds.Status.DesiredNumberScheduled, ds.Status.UpdatedNumberScheduled, ds.Status.NumberAvailable = 1, 1, 1
	if err := c.Status().Update(ctx, ds); err != nil {
		t.Fatalf("set DaemonSet status: %v", err)
	}
}

// TestEnvtestInstallSetCleanNamespaceLifecycle: the
// namespace precondition of a CLEAN install. On a fresh plane the operand
// namespace does not exist; the admin applies the committed install set
// (namespace → RBAC) ONCE — kubectl-apply semantics over the committed
// files — and the CR create then drives the token-only manager through:
// RBACDrift gate healthy (no admin second apply), operand apply, workload
// gate, and Ready. The old failure mode was that the install set carried
// only the RBAC objects, so either the namespaced Role/RoleBinding apply
// failed against a missing namespace, or the operator-created-namespace
// path raced the RBACDrift gate and the setup needed a second admin pass.
func TestEnvtestInstallSetCleanNamespaceLifecycle(t *testing.T) {
	startRBACPlaneManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()

	// Precondition: fresh plane (file ordering contract above).
	ns := &corev1.Namespace{}
	if err := rbacAdmin.Get(ctx, types.NamespacedName{Name: "ngxstorage-nfs"}, ns); !apierrors.IsNotFound(err) {
		t.Fatalf("precondition violated: operand namespace already exists (this test must run first on the RBAC plane): %v", err)
	}

	// ONE admin apply of the committed install set — namespace first.
	if err := applyCommittedInstallSet(rbacAdmin, v1alpha1.ProfileNFS); err != nil {
		t.Fatalf("admin install-set apply: %v", err)
	}
	if getNamespace(rbacAdmin, "ngxstorage-nfs") == nil {
		t.Fatal("install set must create the operand namespace")
	}
	probe := &rbacv1.ClusterRole{}
	if err := rbacAdmin.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller"}, probe); err != nil {
		t.Fatalf("install set must create the operand ClusterRole: %v", err)
	}

	// CR create — the token-only manager reconciles from its watch.
	createSingletonOn(t, rbacAdmin)
	pollStatusOn(t, rbacAdmin, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	})
	q := &v1alpha1.Quartet{}
	if err := rbacAdmin.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	if c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady); c.Reason != v1alpha1.ReasonWorkloadNotReady {
		t.Fatalf("first settled reason = %q (message %s), want WorkloadNotReady — a missing install set would surface RBACDrift instead", c.Reason, c.Message)
	}
	// The RBACDrift gate passed with the single admin apply: the operand
	// set exists and is owned.
	dep := &appsv1.Deployment{}
	if err := rbacAdmin.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller", Namespace: "ngxstorage-nfs"}, dep); err != nil {
		t.Fatalf("operands must apply behind a healthy RBACDrift gate: %v", err)
	}
	if dep.Annotations[renderer.OwnerUIDAnnotation] == "" {
		t.Fatal("operand Deployment must be owner-annotated")
	}

	// Workload convergence → Ready, still with exactly one admin apply.
	convergeWorkloadsOn(t, rbacAdmin)
	pollStatusOn(t, rbacAdmin, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue)
	})
	t.Log("PASS: committed install set (namespace → RBAC) applied once by admin; token-only manager reached Ready with no second apply")
}

// TestEnvtestRBACDriftWatchTriggersReconcile: after the
// profile is READY, drift of the installer-owned RBAC set must trigger a
// reconcile ON ITS OWN — no CR event, no owned-child event. The manager's
// fixed profile/kind/name filtered read-only watch (watchedInstallRBAC)
// enqueues the singleton on pinned-name events; verifyOperandRBAC then
// reports RBACDrift and gates operand apply. Admin restore of the install
// set (kubectl-apply semantics) recovers to Ready. The whole flow runs on
// the token-only SA: the watch itself must authorize against role.yaml.
func TestEnvtestRBACDriftWatchTriggersReconcile(t *testing.T) {
	startRBACPlaneManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*suiteTimeout)
	defer cancel()

	// The install-set lifecycle test leaves a Ready singleton behind; reset
	// to a clean CR so this test's Ready→drift→Ready arc is self-contained.
	resetSingletonOn(t, rbacAdmin)
	if err := applyCommittedInstallSet(rbacAdmin, v1alpha1.ProfileNFS); err != nil {
		t.Fatalf("seed install set: %v", err)
	}
	createSingletonOn(t, rbacAdmin)
	// Reach Ready exactly once (the pre-drift steady state: a successful
	// reconcile returns an empty Result).
	pollStatusOn(t, rbacAdmin, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	})
	convergeWorkloadsOn(t, rbacAdmin)
	pollStatusOn(t, rbacAdmin, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue)
	})

	dep := &appsv1.Deployment{}
	if err := rbacAdmin.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller", Namespace: "ngxstorage-nfs"}, dep); err != nil {
		t.Fatal(err)
	}
	rvBefore := dep.ResourceVersion

	// Poll on reason AND message: the condition may already carry
	// RBACDrift from an earlier drift step, so the message — which names
	// the CURRENTLY drifted objects — is what advances between steps.
	wantDrift := func(messageMustContain string) {
		t.Helper()
		pollStatusOn(t, rbacAdmin, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
			for _, c := range q.Status.Conditions {
				if c.Type == v1alpha1.ConditionReady && c.Reason == v1alpha1.ReasonRBACDrift &&
					strings.Contains(c.Message, messageMustContain) {
					return true
				}
			}
			return false
		})
		// The gate precedes operand writes: nothing modified.
		after := &appsv1.Deployment{}
		if err := rbacAdmin.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller", Namespace: "ngxstorage-nfs"}, after); err != nil {
			t.Fatal(err)
		}
		if after.ResourceVersion != rvBefore {
			t.Fatalf("RBACDrift gate must not modify operands (resourceVersion %s -> %s)", rvBefore, after.ResourceVersion)
		}
	}

	// (1) RULES EDIT on the pinned install ClusterRole — only the RBAC
	// watch can turn this into a reconcile (the edit's event is an RBAC-
	// kind event, and the shared operand enqueuer would drop it: the
	// object carries no ownership annotation).
	cr := &rbacv1.ClusterRole{}
	if err := rbacAdmin.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller"}, cr); err != nil {
		t.Fatal(err)
	}
	cr.Rules = append(cr.Rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create"}})
	if err := rbacAdmin.Update(ctx, cr); err != nil {
		t.Fatalf("drift edit: %v", err)
	}
	wantDrift("")
	t.Log("PASS: rules edit with no CR/child event triggered RBACDrift via the install-owned RBAC watch")

	// (2) DELETE of a pinned install Role.
	if err := rbacAdmin.Delete(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-leader-election", Namespace: "ngxstorage-nfs"}}); err != nil {
		t.Fatalf("drift delete: %v", err)
	}
	wantDrift("not installed")
	t.Log("PASS: install Role delete triggered RBACDrift (object-not-installed)")

	// (3) ADMIN RESTORE: re-apply the committed install set (kubectl-apply
	// semantics overwrite the edited ClusterRole and recreate the deleted
	// Role). The restore's own events drive the recovery reconcile.
	if err := applyCommittedInstallSet(rbacAdmin, v1alpha1.ProfileNFS); err != nil {
		t.Fatalf("admin restore: %v", err)
	}
	pollStatusOn(t, rbacAdmin, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue)
	})
	// The restore healed the edited rule: the drift edit added one rule, so
	// the committed set has one fewer than the drifted cr.
	crAfter := &rbacv1.ClusterRole{}
	if err := rbacAdmin.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller"}, crAfter); err != nil {
		t.Fatal(err)
	}
	if len(crAfter.Rules) != len(cr.Rules)-1 {
		t.Fatalf("restore must heal the ClusterRole to the committed rules (got %d, want %d)", len(crAfter.Rules), len(cr.Rules)-1)
	}
	t.Log("PASS: admin restore of the install set recovered Ready with no further admin step")
}
