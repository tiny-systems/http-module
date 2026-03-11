package portmanager

import (
	"context"
	"os"
	"testing"

	v1alpha1 "github.com/tiny-systems/module/api/v1alpha1"
	v1core "k8s.io/api/core/v1"
	v1ingress "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace   = "default"
	testReleaseName = "my-release"
	testPodName     = "my-release-pod-0"
	testServiceName = "my-release-tinysystems-operator-manager"
	testIngressName = "my-release-tinysystems-operator"
)

// setupTestEnv creates a fake K8s client with a pod, service, and ingress
// that mirror a real TinySystems deployment. Returns the Manager and cleanup func.
func setupTestEnv(t *testing.T, svcPorts []v1core.ServicePort, ingressRules []v1ingress.IngressRule, ingressTLS []v1ingress.IngressTLS) *Manager {
	t.Helper()

	os.Setenv("HOSTNAME", testPodName)
	t.Cleanup(func() { os.Unsetenv("HOSTNAME") })

	pod := &v1core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPodName,
			Namespace: testNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/instance": testReleaseName,
			},
		},
	}

	svc := &v1core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testServiceName,
			Namespace: testNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/instance":  testReleaseName,
				"app.kubernetes.io/name":      "tinysystems-operator",
				"app.kubernetes.io/component": "manager",
			},
		},
		Spec: v1core.ServiceSpec{
			Ports: svcPorts,
		},
	}

	pathType := v1ingress.PathTypePrefix
	ingress := &v1ingress.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testIngressName,
			Namespace: testNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/instance": testReleaseName,
				"app.kubernetes.io/name":     "tinysystems-operator",
			},
			Annotations: map[string]string{
				v1alpha1.IngressHostNameSuffixAnnotation: "app.tinysystems.io",
			},
		},
		Spec: v1ingress.IngressSpec{
			Rules: ingressRules,
			TLS:   ingressTLS,
			// Add a default rule so the ingress isn't empty
			DefaultBackend: &v1ingress.IngressBackend{
				Service: &v1ingress.IngressServiceBackend{
					Name: testServiceName,
					Port: v1ingress.ServiceBackendPort{Number: 8080},
				},
			},
		},
	}

	// Ensure default rules include the standard path type if provided
	_ = pathType

	scheme := runtime.NewScheme()
	_ = v1core.AddToScheme(scheme)
	_ = v1ingress.AddToScheme(scheme)

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod, svc, ingress).
		Build()

	return New(cl, testNamespace)
}

// TestExposeAndDisclose_Basic verifies basic expose/disclose works.
func TestExposeAndDisclose_Basic(t *testing.T) {
	mgr := setupTestEnv(t, nil, nil, nil)
	ctx := context.Background()

	// Expose port 40753 with hostname
	urls, err := mgr.ExposePort(ctx, "my-server", nil, 40753)
	if err != nil {
		t.Fatalf("ExposePort failed: %v", err)
	}
	if len(urls) == 0 {
		t.Fatal("ExposePort returned no URLs")
	}

	// Verify service has the port
	svc, err := mgr.getReleaseService(ctx, testReleaseName)
	if err != nil {
		t.Fatalf("getReleaseService failed: %v", err)
	}
	if !hasServicePort(svc, 40753) {
		t.Fatal("service should have port 40753 after ExposePort")
	}

	// Verify ingress has a rule
	ingress, err := mgr.getReleaseIngress(ctx, testReleaseName)
	if err != nil {
		t.Fatalf("getReleaseIngress failed: %v", err)
	}
	if !hasIngressRuleForPort(ingress, testServiceName, 40753) {
		t.Fatal("ingress should have rule for port 40753 after ExposePort")
	}

	// Disclose
	if err := mgr.DisclosePort(ctx, 40753); err != nil {
		t.Fatalf("DisclosePort failed: %v", err)
	}

	// Verify service no longer has the port
	svc, err = mgr.getReleaseService(ctx, testReleaseName)
	if err != nil {
		t.Fatalf("getReleaseService failed: %v", err)
	}
	if hasServicePort(svc, 40753) {
		t.Fatal("service should NOT have port 40753 after DisclosePort")
	}

	// Verify ingress no longer has the rule
	ingress, err = mgr.getReleaseIngress(ctx, testReleaseName)
	if err != nil {
		t.Fatalf("getReleaseIngress failed: %v", err)
	}
	if hasIngressRuleForPort(ingress, testServiceName, 40753) {
		t.Fatal("ingress should NOT have rule for port 40753 after DisclosePort")
	}
}

// TestDisclosePort_RemovesRulesAddedByAnotherPod reproduces the actual production bug:
//
// Scenario (module upgrade / rolling restart):
//  1. New pod starts, calls ExposePort — adds port 40753 to service, adds ingress rule for status.tinysystems.io
//  2. Old pod shuts down, calls DisclosePort — reads STALE service/ingress (from before step 1),
//     removes port 40753 and the ingress rule
//  3. Result: port and rule are gone, 503 error
//
// The bug is that discloseServicePort and removeRulesIngress read the resource once
// (at the start of DisclosePort) and then update without re-reading, so they operate
// on stale data and overwrite the new pod's changes.
func TestDisclosePort_RemovesRulesAddedByAnotherPod(t *testing.T) {
	// Start with port 40753 already exposed (the new pod's ExposePort already ran)
	existingPort := v1core.ServicePort{
		Name:       "port40753",
		Port:       40753,
		TargetPort: intstr.FromInt32(40753),
		Protocol:   v1core.ProtocolTCP,
	}

	pathType := v1ingress.PathTypePrefix
	existingRule := v1ingress.IngressRule{
		Host: "status.tinysystems.io",
		IngressRuleValue: v1ingress.IngressRuleValue{
			HTTP: &v1ingress.HTTPIngressRuleValue{
				Paths: []v1ingress.HTTPIngressPath{
					{
						Path:     "/",
						PathType: &pathType,
						Backend: v1ingress.IngressBackend{
							Service: &v1ingress.IngressServiceBackend{
								Name: testServiceName,
								Port: v1ingress.ServiceBackendPort{Number: 40753},
							},
						},
					},
				},
			},
		},
	}
	existingTLS := v1ingress.IngressTLS{
		Hosts:      []string{"status.tinysystems.io"},
		SecretName: "status.tinysystems.io-tls",
	}

	mgr := setupTestEnv(t,
		[]v1core.ServicePort{existingPort},
		[]v1ingress.IngressRule{existingRule},
		[]v1ingress.IngressTLS{existingTLS},
	)

	ctx := context.Background()

	// Simulate old pod: it reads the service and ingress (gets stale copies)
	// Then the new pod's ExposePort has already written (the state above).
	// Now old pod calls DisclosePort for the same port.
	//
	// In production, the old pod's DisclosePort reads the CURRENT state (which includes
	// the new pod's additions) and removes the port — this is the bug.
	// DisclosePort SHOULD NOT remove ports/rules that another pod legitimately added.
	//
	// For now, let's just verify: after DisclosePort, are the rules gone?
	err := mgr.DisclosePort(ctx, 40753)
	if err != nil {
		t.Fatalf("DisclosePort failed: %v", err)
	}

	// Check: the old pod's DisclosePort removed the new pod's ingress rule and service port.
	// This is the BUG — after the fix, the old pod should NOT be calling DisclosePort at all.
	svc, err := mgr.getReleaseService(ctx, testReleaseName)
	if err != nil {
		t.Fatalf("getReleaseService failed: %v", err)
	}

	ingress, err := mgr.getReleaseIngress(ctx, testReleaseName)
	if err != nil {
		t.Fatalf("getReleaseIngress failed: %v", err)
	}

	// THIS IS THE FAILING ASSERTION — demonstrates the bug.
	// The port and rule SHOULD still be present (the server is still running on the new pod),
	// but DisclosePort removed them because nothing prevents it.
	//
	// The fix: non-leader pods must not call DisclosePort on shared resources.
	// Only the leader (who coordinated the ExposePort) should clean up.
	if !hasServicePort(svc, 40753) {
		t.Error("BUG: DisclosePort removed service port 40753 that was added by another pod. " +
			"After fix, non-leader pods should not call DisclosePort.")
	}
	if !hasIngressRuleForPort(ingress, testServiceName, 40753) {
		t.Error("BUG: DisclosePort removed ingress rule for port 40753 that was added by another pod. " +
			"After fix, non-leader pods should not call DisclosePort.")
	}
}

// TestExposePort_ConcurrentCallsPreserveData verifies that two concurrent
// ExposePort calls for different ports don't lose each other's data.
// This works correctly today thanks to retry-on-conflict in exposeServicePort
// and addRulesIngress.
func TestExposePort_ConcurrentCallsPreserveData(t *testing.T) {
	mgr := setupTestEnv(t, nil, nil, nil)
	ctx := context.Background()

	// Expose two different ports sequentially (fake client is synchronous,
	// but this verifies the read-modify-write logic doesn't lose data)
	_, err := mgr.ExposePort(ctx, "server-a", nil, 40001)
	if err != nil {
		t.Fatalf("ExposePort(40001) failed: %v", err)
	}

	_, err = mgr.ExposePort(ctx, "server-b", nil, 40002)
	if err != nil {
		t.Fatalf("ExposePort(40002) failed: %v", err)
	}

	// Both ports should exist on the service
	svc, err := mgr.getReleaseService(ctx, testReleaseName)
	if err != nil {
		t.Fatalf("getReleaseService failed: %v", err)
	}
	if !hasServicePort(svc, 40001) {
		t.Error("service should have port 40001")
	}
	if !hasServicePort(svc, 40002) {
		t.Error("service should have port 40002")
	}

	// Both ingress rules should exist
	ingress, err := mgr.getReleaseIngress(ctx, testReleaseName)
	if err != nil {
		t.Fatalf("getReleaseIngress failed: %v", err)
	}
	if !hasIngressRuleForPort(ingress, testServiceName, 40001) {
		t.Error("ingress should have rule for port 40001")
	}
	if !hasIngressRuleForPort(ingress, testServiceName, 40002) {
		t.Error("ingress should have rule for port 40002")
	}
}

// TestDisclosePort_OnlyRemovesOwnPort verifies that DisclosePort for one port
// does not remove a different port's service entry or ingress rule.
func TestDisclosePort_OnlyRemovesOwnPort(t *testing.T) {
	port1 := v1core.ServicePort{
		Name: "port40001", Port: 40001,
		TargetPort: intstr.FromInt32(40001), Protocol: v1core.ProtocolTCP,
	}
	port2 := v1core.ServicePort{
		Name: "port40002", Port: 40002,
		TargetPort: intstr.FromInt32(40002), Protocol: v1core.ProtocolTCP,
	}

	pathType := v1ingress.PathTypePrefix
	rule1 := v1ingress.IngressRule{
		Host: "server-a-app.tinysystems.io",
		IngressRuleValue: v1ingress.IngressRuleValue{
			HTTP: &v1ingress.HTTPIngressRuleValue{
				Paths: []v1ingress.HTTPIngressPath{{
					Path: "/", PathType: &pathType,
					Backend: v1ingress.IngressBackend{
						Service: &v1ingress.IngressServiceBackend{
							Name: testServiceName,
							Port: v1ingress.ServiceBackendPort{Number: 40001},
						},
					},
				}},
			},
		},
	}
	rule2 := v1ingress.IngressRule{
		Host: "server-b-app.tinysystems.io",
		IngressRuleValue: v1ingress.IngressRuleValue{
			HTTP: &v1ingress.HTTPIngressRuleValue{
				Paths: []v1ingress.HTTPIngressPath{{
					Path: "/", PathType: &pathType,
					Backend: v1ingress.IngressBackend{
						Service: &v1ingress.IngressServiceBackend{
							Name: testServiceName,
							Port: v1ingress.ServiceBackendPort{Number: 40002},
						},
					},
				}},
			},
		},
	}

	mgr := setupTestEnv(t,
		[]v1core.ServicePort{port1, port2},
		[]v1ingress.IngressRule{rule1, rule2},
		nil,
	)

	ctx := context.Background()

	// Disclose only port 40001
	if err := mgr.DisclosePort(ctx, 40001); err != nil {
		t.Fatalf("DisclosePort(40001) failed: %v", err)
	}

	// Port 40001 should be gone
	svc, _ := mgr.getReleaseService(ctx, testReleaseName)
	if hasServicePort(svc, 40001) {
		t.Error("service should NOT have port 40001 after disclose")
	}

	// Port 40002 should still be there
	if !hasServicePort(svc, 40002) {
		t.Error("service should still have port 40002")
	}

	ingress, _ := mgr.getReleaseIngress(ctx, testReleaseName)
	if hasIngressRuleForPort(ingress, testServiceName, 40001) {
		t.Error("ingress should NOT have rule for port 40001 after disclose")
	}
	if !hasIngressRuleForPort(ingress, testServiceName, 40002) {
		t.Error("ingress should still have rule for port 40002")
	}
}

func hasServicePort(svc *v1core.Service, port int) bool {
	for _, p := range svc.Spec.Ports {
		if p.Port == int32(port) {
			return true
		}
	}
	return false
}

func hasIngressRuleForPort(ingress *v1ingress.Ingress, serviceName string, port int) bool {
	for _, rule := range ingress.Spec.Rules {
		if rule.IngressRuleValue.HTTP == nil {
			continue
		}
		for _, path := range rule.IngressRuleValue.HTTP.Paths {
			if path.Backend.Service != nil &&
				path.Backend.Service.Name == serviceName &&
				path.Backend.Service.Port.Number == int32(port) {
				return true
			}
		}
	}
	return false
}
