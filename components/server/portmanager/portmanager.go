package portmanager

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/tiny-systems/module/api/v1alpha1"
	v1core "k8s.io/api/core/v1"
	v1ingress "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
	maxRetries := 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		// Always fetch fresh service to avoid race with other replicas
		freshSvc := &v1core.Service{}
		if err := m.client.Get(ctx, client.ObjectKey{Namespace: svc.Namespace, Name: svc.Name}, freshSvc); err != nil {
			return fmt.Errorf("failed to get service: %w", err)
		}

		// Check if port already exists
		for _, p := range freshSvc.Spec.Ports {
			if p.Port == int32(port) {
				log.Info().Int("port", port).Msg("portmanager: port already exposed on service")
				return nil
			}
		}

		// Add port
		freshSvc.Spec.Ports = append(freshSvc.Spec.Ports, v1core.ServicePort{
			Name:       fmt.Sprintf("port%d", port),
			Port:       int32(port),
			TargetPort: intstr.FromInt32(int32(port)),
			Protocol:   v1core.ProtocolTCP,
		})

		log.Info().Int("port", port).Str("service", svc.Name).Int("attempt", attempt+1).Msg("portmanager: updating service")

		err := m.client.Update(ctx, freshSvc)
		if err == nil {
			log.Info().Int("port", port).Str("service", svc.Name).Msg("portmanager: service port added")
			return nil
		}

		if errors.IsConflict(err) {
			log.Warn().Int("port", port).Int("attempt", attempt+1).Msg("portmanager: conflict, retrying")
			time.Sleep(time.Millisecond * 100 * time.Duration(attempt+1))
			continue
		}

		return fmt.Errorf("failed to update service: %w", err)
	}

	return fmt.Errorf("failed to add port after %d retries", maxRetries)
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

	// Retry loop for conflict errors
	maxRetries := 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Re-fetch ingress on retry to get latest version
			time.Sleep(time.Millisecond * 100 * time.Duration(attempt))
			freshIngress, err := m.getReleaseIngressByName(ctx, ingress.Name)
			if err != nil {
				log.Error().Err(err).Msg("portmanager: failed to re-fetch ingress")
				return []string{}, err
			}
			ingress = freshIngress
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

		log.Info().Str("ingress", ingress.Name).Int("rulesCount", len(ingress.Spec.Rules)).Msg("portmanager: updating ingress")
		err := m.client.Update(ctx, ingress)
		if err == nil {
			log.Info().Str("ingress", ingress.Name).Strs("hostnames", hostnames).Msg("portmanager: ingress updated successfully")
			return hostnames, nil
		}

		if errors.IsConflict(err) {
			log.Warn().Int("attempt", attempt+1).Msg("portmanager: ingress update conflict, retrying")
			continue
		}

		log.Error().Err(err).Str("ingress", ingress.Name).Msg("portmanager: failed to update ingress")
		return []string{}, err
	}

	return []string{}, fmt.Errorf("failed to update ingress after %d retries", maxRetries)
}

func (m *Manager) getReleaseIngressByName(ctx context.Context, name string) (*v1ingress.Ingress, error) {
	ingress := &v1ingress.Ingress{}
	err := m.client.Get(ctx, client.ObjectKey{
		Namespace: m.namespace,
		Name:      name,
	}, ingress)
	if err != nil {
		return nil, err
	}
	return ingress, nil
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
