package portmanager

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/tiny-systems/module/api/v1alpha1"
	v1core "k8s.io/api/core/v1"
	v1ingress "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Manager handles Kubernetes Service and Ingress operations for port exposure
type Manager struct {
	client    client.WithWatch
	namespace string
	lock      *sync.Mutex
}

// New creates a new port manager
func New(c client.WithWatch, namespace string) *Manager {
	return &Manager{
		client:    c,
		namespace: namespace,
		lock:      &sync.Mutex{},
	}
}

// ExposePort exposes a port on the Service and adds Ingress rules for the given hostnames
func (m *Manager) ExposePort(ctx context.Context, autoHostName string, hostnames []string, port int) ([]string, error) {
	currentPod := os.Getenv("HOSTNAME")
	if currentPod == "" {
		return []string{}, fmt.Errorf("unable to determine the current pod's name")
	}
	if hostnames == nil {
		hostnames = []string{}
	}
	if len(hostnames) == 0 && autoHostName == "" {
		return []string{}, fmt.Errorf("empty hostnames provided")
	}

	releaseName, err := m.getReleaseNameByPodName(ctx, currentPod)
	if err != nil {
		return nil, fmt.Errorf("unable to find release name: %v", err)
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	svc, err := m.getReleaseService(ctx, releaseName)
	if err != nil {
		return []string{}, fmt.Errorf("unable to get service: %v", err)
	}

	ingress, _ := m.getReleaseIngress(ctx, releaseName)

	if ingress == nil {
		return []string{}, fmt.Errorf("no ingress")
	}

	if err = m.exposeServicePort(ctx, svc, port); err != nil {
		return []string{}, err
	}

	prefix := m.getIngressAutoHostnamePrefix(ingress)

	if prefix != "" && autoHostName != "" {
		hostnames = append(hostnames, fmt.Sprintf("%s-%s", autoHostName, prefix))
	}

	return m.addRulesIngress(ctx, ingress, svc, hostnames, port)
}

// DisclosePort removes a port from the Service and removes Ingress rules
func (m *Manager) DisclosePort(ctx context.Context, port int) error {
	currentPod := os.Getenv("HOSTNAME")
	if currentPod == "" {
		return nil
	}

	releaseName, err := m.getReleaseNameByPodName(ctx, currentPod)
	if err != nil {
		return fmt.Errorf("unable to find release name: %v", err)
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	svc, err := m.getReleaseService(ctx, releaseName)
	if err != nil {
		return fmt.Errorf("unable to get service: %v", err)
	}

	ingress, _ := m.getReleaseIngress(ctx, releaseName)

	if ingress == nil {
		return fmt.Errorf("no ingress")
	}

	if err = m.discloseServicePort(ctx, svc, port); err != nil {
		return err
	}
	return m.removeRulesIngress(ctx, ingress, svc, port)
}

func (m *Manager) getReleaseNameByPodName(ctx context.Context, podName string) (string, error) {
	pod := &v1core.Pod{}
	err := m.client.Get(ctx, client.ObjectKey{
		Namespace: m.namespace,
		Name:      podName,
	}, pod)

	if err != nil {
		return "", fmt.Errorf("unable to find current pod: %v", err)
	}

	var releaseName string
	for k, v := range pod.ObjectMeta.Labels {
		if k == "app.kubernetes.io/instance" {
			releaseName = v
		}
	}
	if releaseName == "" {
		return "", fmt.Errorf("release name label not found")
	}
	return releaseName, nil
}

func (m *Manager) getReleaseService(ctx context.Context, releaseName string) (*v1core.Service, error) {
	servicesList := &v1core.ServiceList{}
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app.kubernetes.io/instance":  releaseName,
			"app.kubernetes.io/name":      "tinysystems-operator",
			"app.kubernetes.io/component": "manager",
		},
	})

	if err != nil {
		return nil, fmt.Errorf("build service selector error: %s", err)
	}

	if err = m.client.List(ctx, servicesList, client.MatchingLabelsSelector{
		Selector: selector,
	}, client.InNamespace(m.namespace)); err != nil {
		return nil, fmt.Errorf("service list error: %v", err)
	}

	if len(servicesList.Items) == 0 {
		return nil, fmt.Errorf("unable to find manager service")
	}

	if len(servicesList.Items) > 1 {
		return nil, fmt.Errorf("service is ambiguous")
	}

	return &servicesList.Items[0], nil
}

func (m *Manager) getReleaseIngress(ctx context.Context, releaseName string) (*v1ingress.Ingress, error) {
	ingressList := &v1ingress.IngressList{}
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app.kubernetes.io/instance": releaseName,
			"app.kubernetes.io/name":     "tinysystems-operator",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build ingress selector error: %s", err)
	}

	if err = m.client.List(ctx, ingressList, client.MatchingLabelsSelector{
		Selector: selector,
	}, client.InNamespace(m.namespace)); err != nil {
		return nil, fmt.Errorf("ingress list error: %v", err)
	}

	if len(ingressList.Items) == 0 {
		return nil, fmt.Errorf("unable to find manager ingress")
	}

	if len(ingressList.Items) > 1 {
		return nil, fmt.Errorf("ingress is ambiguous")
	}
	return &ingressList.Items[0], nil
}

func (m *Manager) getIngressAutoHostnamePrefix(ingress *v1ingress.Ingress) string {
	var hostNamePrefix string
	for k, v := range ingress.Annotations {
		if k == v1alpha1.IngressHostNameSuffixAnnotation {
			hostNamePrefix = v
		}
	}
	return hostNamePrefix
}

func (m *Manager) exposeServicePort(ctx context.Context, svc *v1core.Service, port int) error {
	for _, p := range svc.Spec.Ports {
		if p.Port == int32(port) {
			return nil
		}
	}
	svc.Spec.Ports = append(svc.Spec.Ports, v1core.ServicePort{
		Name:       fmt.Sprintf("port%d", port),
		Port:       int32(port),
		TargetPort: intstr.FromInt32(int32(port)),
	})
	return m.client.Update(ctx, svc)
}

func (m *Manager) discloseServicePort(ctx context.Context, svc *v1core.Service, port int) error {
	var ports []v1core.ServicePort

	for _, p := range svc.Spec.Ports {
		if p.Port == int32(port) {
			continue
		}
		ports = append(ports, p)
	}
	svc.Spec.Ports = ports
	return m.client.Update(ctx, svc)
}

func (m *Manager) addRulesIngress(ctx context.Context, ingress *v1ingress.Ingress, service *v1core.Service, hostnames []string, port int) ([]string, error) {
	if len(hostnames) == 0 {
		return []string{}, fmt.Errorf("no hostnames provided")
	}
	pathType := v1ingress.PathTypePrefix

	rule := v1ingress.IngressRuleValue{
		HTTP: &v1ingress.HTTPIngressRuleValue{
			Paths: []v1ingress.HTTPIngressPath{
				{
					Path:     "/",
					PathType: &pathType,
					Backend: v1ingress.IngressBackend{
						Service: &v1ingress.IngressServiceBackend{
							Name: service.Name,
							Port: v1ingress.ServiceBackendPort{
								Number: int32(port),
							},
						},
					},
				},
			},
		},
	}

INGRESS:
	for _, hostname := range hostnames {
		for idx, r := range ingress.Spec.Rules {
			if r.Host != hostname {
				continue
			}
			ingress.Spec.Rules[idx].IngressRuleValue = rule
			continue INGRESS
		}
		ingress.Spec.Rules = append(ingress.Spec.Rules, v1ingress.IngressRule{
			Host:             hostname,
			IngressRuleValue: rule,
		})
	}

	var newHostNames []string

HOSTNAMES:
	for _, hostname := range hostnames {
		for _, t := range ingress.Spec.TLS {
			for _, th := range t.Hosts {
				if th != hostname {
					continue
				}
				continue HOSTNAMES
			}
		}
		newHostNames = append(newHostNames, hostname)
	}

	if len(newHostNames) > 0 {
		for _, hostname := range newHostNames {
			ingress.Spec.TLS = append(ingress.Spec.TLS, v1ingress.IngressTLS{
				Hosts:      []string{hostname},
				SecretName: fmt.Sprintf("%s-tls", hostname),
			})
		}
	}

	if err := m.client.Update(ctx, ingress); err != nil {
		return []string{}, err
	}
	return hostnames, nil
}

func (m *Manager) removeRulesIngress(ctx context.Context, ingress *v1ingress.Ingress, service *v1core.Service, port int) error {
	var (
		rules            []v1ingress.IngressRule
		tls              []v1ingress.IngressTLS
		deletedHostnames []string
	)

RULES:
	for _, rule := range ingress.Spec.Rules {
		if rule.IngressRuleValue.HTTP == nil {
			continue
		}
		for _, p := range rule.IngressRuleValue.HTTP.Paths {
			if p.Backend.Service.Port.Number == int32(port) && p.Backend.Service.Name == service.Name {
				deletedHostnames = append(deletedHostnames, rule.Host)
				continue RULES
			}
			rules = append(rules, rule)
		}
	}

	for _, t := range ingress.Spec.TLS {
		var found bool
		for _, h := range t.Hosts {
			for _, host := range deletedHostnames {
				if h == host {
					found = true
				}
			}
		}
		if found {
			continue
		}
		tls = append(tls, t)
	}

	ingress.Spec.TLS = tls
	ingress.Spec.Rules = rules
	return m.client.Update(ctx, ingress)
}
