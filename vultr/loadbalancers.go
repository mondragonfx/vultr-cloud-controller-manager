// Package vultr is vultr cloud specific implementation
package vultr

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/asaskevich/govalidator"
	"github.com/vultr/govultr/v3"
	"github.com/vultr/metadata"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
)

const (
	// annoVultrLoadBalancerLabel is used to set custom labels for load balancers
	annoVultrLoadBalancerLabel = "service.beta.kubernetes.io/vultr-loadbalancer-label"

	// annoVultrLoadBalancerID is used to identify individual Vultr load balancers, this is managed by the CCM
	annoVultrLoadBalancerID = "service.beta.kubernetes.io/vultr-loadbalancer-id"

	// annoVultrLoadBalancerCreate defaults to true and is to specify whether or not to create a VLB for the svc
	annoVultrLoadBalancerCreate = "service.beta.kubernetes.io/vultr-loadbalancer-create"

	// annoVultrLBProtocol is the annotation used to specify
	// which protocol should be used for a Load Balancer.
	// Note that if annoVultrLBHTTPSPorts is defined then this will be overridden to HTTPS
	annoVultrLBProtocol = "service.beta.kubernetes.io/vultr-loadbalancer-protocol"

	// annoVultrLBHTTPSPorts is the annotation used to specify
	// which ports should be used for HTTPS.
	// You can pass in a comma separated list: 443,8443
	annoVultrLbHTTPSPorts = "service.beta.kubernetes.io/vultr-loadbalancer-https-ports"

	// annoVultrLBSSLPassthrough is the annotation used to specify
	// whether or not you do not wish to have SSL termination on the load balancer
	// default is false to enable set to true
	annoVultrLBSSLPassthrough = "service.beta.kubernetes.io/vultr-loadbalancer-ssl-pass-through" //nolint

	// annoVultrLBSSL is the annotation used to specify
	// which TLS secret you want to be used for your load balancers SSL
	annoVultrLBSSL = "service.beta.kubernetes.io/vultr-loadbalancer-ssl"

	// annoVultrLBBackendProtocol backend protocol
	annoVultrLBBackendProtocol = "service.beta.kubernetes.io/vultr-loadbalancer-backend-protocol"

	// annoVultrHostname is the hostname used for VLB to prevent hairpinning
	annoVultrHostname = "service.beta.kubernetes.io/vultr-loadbalancer-hostname"

	annoVultrHealthCheckPath               = "service.beta.kubernetes.io/vultr-loadbalancer-healthcheck-path"
	annoVultrHealthCheckProtocol           = "service.beta.kubernetes.io/vultr-loadbalancer-healthcheck-protocol"
	annoVultrHealthCheckPort               = "service.beta.kubernetes.io/vultr-loadbalancer-healthcheck-port"
	annoVultrHealthCheckInterval           = "service.beta.kubernetes.io/vultr-loadbalancer-healthcheck-check-interval"
	annoVultrHealthCheckResponseTimeout    = "service.beta.kubernetes.io/vultr-loadbalancer-healthcheck-response-timeout"
	annoVultrHealthCheckUnhealthyThreshold = "service.beta.kubernetes.io/vultr-loadbalancer-healthcheck-unhealthy-threshold"
	annoVultrHealthCheckHealthyThreshold   = "service.beta.kubernetes.io/vultr-loadbalancer-healthcheck-healthy-threshold"

	annoVultrAlgorithm     = "service.beta.kubernetes.io/vultr-loadbalancer-algorithm"
	annoVultrSSLRedirect   = "service.beta.kubernetes.io/vultr-loadbalancer-ssl-redirect"
	annoVultrProxyProtocol = "service.beta.kubernetes.io/vultr-loadbalancer-proxy-protocol"
	annoVultrLBHTTP2       = "service.beta.kubernetes.io/vultr-loadbalancer-http2"
	annoVultrLBHTTP3       = "service.beta.kubernetes.io/vultr-loadbalancer-http3"
	annoVultrLBTimeout     = "service.beta.kubernetes.io/vultr-loadbalancer-timeout"

	annoVultrStickySessionEnabled    = "service.beta.kubernetes.io/vultr-loadbalancer-sticky-session-enabled"
	annoVultrStickySessionCookieName = "service.beta.kubernetes.io/vultr-loadbalancer-sticky-session-cookie-name"

	annoVultrFirewallRules  = "service.beta.kubernetes.io/vultr-loadbalancer-firewall-rules"
	annoVultrPrivateNetwork = "service.beta.kubernetes.io/vultr-loadbalancer-private-network"
	annoVultrVPC            = "service.beta.kubernetes.io/vultr-loadbalancer-vpc"

	annoVultrNodeCount = "service.beta.kubernetes.io/vultr-loadbalancer-node-count"

	// annoVultrLBSSLLastUpdatedTime is used to keep track of when a SVC is updated due to the SSL secret being updated
	annoVultrLBSSLLastUpdatedTime = "service.beta.kubernetes.io/vultr-loadbalancer-ssl-last-updated"

	// Supported Protocols
	protocolHTTP  = "http"
	protocolHTTPS = "https"
	protocolTCP   = "tcp"

	portProtocolTCP = "TCP" //nolint
	portProtocolUDP = "UDP"

	healthCheckInterval  = 15
	healthCheckResponse  = 5
	healthCheckUnhealthy = 5
	healthCheckHealthy   = 5

	defaultLBTimeout = 600

	lbStatusActive = "active"
)

var errLbNotFound = fmt.Errorf("loadbalancer not found")
var _ cloudprovider.LoadBalancer = &loadbalancers{}

type loadbalancers struct {
	client *govultr.Client
	zone   string

	kubeClient kubernetes.Interface
}

func newLoadbalancers(client *govultr.Client, zone string) cloudprovider.LoadBalancer {
	return &loadbalancers{client: client, zone: zone}
}

func (l *loadbalancers) GetLoadBalancer(ctx context.Context, _ string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	lb, err := l.getVultrLB(ctx, service)
	if err != nil {
		if err == errLbNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}

	enabledIPv6 := checkEnabledIPv6(service)
	var ingress []v1.LoadBalancerIngress
	hostname := lb.Label //nolint

	// Check if hostname annotation is blank and set if not
	if _, ok := service.Annotations[annoVultrHostname]; ok {
		if service.Annotations[annoVultrHostname] != "" {
			if govalidator.IsDNSName(service.Annotations[annoVultrHostname]) {
				hostname = service.Annotations[annoVultrHostname]
			} else {
				return nil, true, fmt.Errorf("hostname %s is not a valid DNS name", service.Annotations[annoVultrHostname])
			}
			klog.Infof("setting hostname for loadbalancer to: %s", hostname)
			ingress = append(ingress, v1.LoadBalancerIngress{Hostname: hostname})
		}
	} else {
		ingress = append(ingress, v1.LoadBalancerIngress{Hostname: hostname, IP: lb.IPV4})

		if enabledIPv6 {
			ingress = append(ingress, v1.LoadBalancerIngress{Hostname: hostname, IP: lb.IPV6})
		}
	}

	return &v1.LoadBalancerStatus{
		Ingress: ingress,
	}, true, nil
}

func (l *loadbalancers) GetLoadBalancerName(_ context.Context, _ string, service *v1.Service) string {
	if label, ok := service.Annotations[annoVultrLoadBalancerLabel]; ok {
		return label
	}
	return getDefaultLBName(service)
}

func getDefaultLBName(service *v1.Service) string {
	return cloudprovider.DefaultLoadBalancerName(service)
}

func (l *loadbalancers) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	_, exists, err := l.GetLoadBalancer(ctx, clusterName, service)
	if err != nil {
		return nil, err
	}

	if create, ok := service.Annotations[annoVultrLoadBalancerCreate]; ok {
		if strings.EqualFold(create, "false") {
			return nil, fmt.Errorf("%s set to %s - load balancer will not be created", annoVultrLoadBalancerCreate, create)
		}
	}

	// if exists is false and the err above was nil then this is errLbNotFound
	if !exists {
		klog.Infof("Load balancer for cluster %q doesn't exist, creating", clusterName)
		lbReq, err1 := l.buildLoadBalancerRequest(service, nodes)
		if err1 != nil {
			return nil, err1
		}

		lbReq.Region = l.zone
		lb2, _, err1 := l.client.LoadBalancer.Create(ctx, lbReq) //nolint:bodyclose
		if err1 != nil {
			return nil, fmt.Errorf("failed to create load-balancer: %s", err1)
		}
		klog.Infof("Created load balancer %q", lb2.ID)

		// Set the Vultr VLB ID annotation
		if _, ok := service.Annotations[annoVultrLoadBalancerID]; !ok {
			if err = l.GetKubeClient(); err != nil {
				return nil, fmt.Errorf("failed to get kubeclient to update service: %s", err)
			}
			service, err = l.kubeClient.CoreV1().Services(service.Namespace).Get(ctx, service.Name, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get service with loadbalancer ID: %s", err)
			}

			if service.Annotations == nil {
				service.Annotations = make(map[string]string)
			}
			service.Annotations[annoVultrLoadBalancerID] = lb2.ID

			_, err = l.kubeClient.CoreV1().Services(service.Namespace).Update(ctx, service, metav1.UpdateOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to update service with loadbalancer ID: %s", err)
			}
		}

		if lb2.Status != lbStatusActive {
			return nil, fmt.Errorf("load-balancer is not yet active - current status: %s", lb2.Status)
		}

		enabledIPv6 := checkEnabledIPv6(service)
		var ingress []v1.LoadBalancerIngress

		hostname := lb2.Label //nolint
		// Check if hostname annotation is blank and set if not
		if _, ok := service.Annotations[annoVultrHostname]; ok {
			if service.Annotations[annoVultrHostname] != "" {
				if govalidator.IsDNSName(service.Annotations[annoVultrHostname]) {
					hostname = service.Annotations[annoVultrHostname]
				} else {
					return nil, fmt.Errorf("hostname %s is not a valid DNS name", service.Annotations[annoVultrHostname])
				}
				klog.Infof("setting hostname for loadbalancer to: %s", hostname)
				ingress = append(ingress, v1.LoadBalancerIngress{Hostname: hostname})
			}
		} else {
			ingress = append(ingress, v1.LoadBalancerIngress{IP: lb2.IPV4})

			if enabledIPv6 {
				ingress = append(ingress, v1.LoadBalancerIngress{IP: lb2.IPV6})
			}
		}

		return &v1.LoadBalancerStatus{
			Ingress: ingress,
		}, nil
	}

	klog.Infof("Load balancer exists for cluster %q", clusterName)

	lb, err := l.getVultrLB(ctx, service)
	if err != nil {
		if err == errLbNotFound {
			return nil, errLbNotFound
		}

		return nil, err
	}

	klog.Infof("Found load balancer: %q", lb.Label)

	// Set the Vultr VLB ID annotation
	if _, ok := service.Annotations[annoVultrLoadBalancerID]; !ok {
		if service.Annotations == nil {
			service.Annotations = make(map[string]string)
		}
		service.Annotations[annoVultrLoadBalancerID] = lb.ID
		if err = l.GetKubeClient(); err != nil {
			return nil, fmt.Errorf("failed to get kubeclient to update service: %s", err)
		}
		_, err = l.kubeClient.CoreV1().Services(service.Namespace).Update(ctx, service, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to update service with loadbalancer ID: %s", err)
		}
	}

	if lb.Status != lbStatusActive {
		return nil, fmt.Errorf("load-balancer is not yet active - current status: %s", lb.Status)
	}

	if err2 := l.UpdateLoadBalancer(ctx, clusterName, service, nodes); err2 != nil {
		return nil, err2
	}

	lbStatus, _, err := l.GetLoadBalancer(ctx, clusterName, service)
	if err != nil {
		return nil, err
	}

	return lbStatus, nil
}

func (l *loadbalancers) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	klog.V(3).Info("Called UpdateLoadBalancers") //nolint
	if _, _, err := l.GetLoadBalancer(ctx, clusterName, service); err != nil {
		return err
	}

	lb, err := l.getVultrLB(ctx, service)
	if err != nil {
		return err
	}

	// Set the Vultr VLB ID annotation
	if _, ok := service.Annotations[annoVultrLoadBalancerID]; !ok {
		service.Annotations[annoVultrLoadBalancerID] = lb.ID
		if err = l.GetKubeClient(); err != nil {
			return fmt.Errorf("failed to get kubeclient to update service: %s", err)
		}
		_, err = l.kubeClient.CoreV1().Services(service.Namespace).Update(ctx, service, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update service with loadbalancer ID: %s", err)
		}
	}

	lbReq, err := l.buildLoadBalancerRequest(service, nodes)
	if err != nil {
		return fmt.Errorf("failed to create load balancer request: %s", err)
	}

	if err := l.client.LoadBalancer.Update(ctx, lb.ID, lbReq); err != nil {
		return fmt.Errorf("failed to update LB: %s", err)
	}

	return nil
}

func (l *loadbalancers) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	_, exists, err := l.GetLoadBalancer(ctx, clusterName, service)
	if err != nil {
		return err
	}
	// This is the same as if we were to check if err == errLbNotFound {
	if !exists {
		return nil
	}

	lb, err := l.getVultrLB(ctx, service)
	if err != nil {
		return err
	}

	err = l.client.LoadBalancer.Delete(ctx, lb.ID)
	if err != nil {
		return err
	}

	return nil
}

func (l *loadbalancers) lbByName(ctx context.Context, lbName string) (*govultr.LoadBalancer, error) {
	listOptions := &govultr.ListOptions{
		PerPage: 25,
	}

	for {
		lbs, meta, _, err := l.client.LoadBalancer.List(ctx, listOptions) //nolint:bodyclose
		if err != nil {
			return nil, err
		}

		for _, v := range lbs { //nolint
			if v.Label == lbName {
				return &v, nil
			}
		}

		if meta.Links.Next == "" {
			break
		}

		listOptions.Cursor = meta.Links.Next
	}

	return nil, errLbNotFound
}

func (l *loadbalancers) lbByID(ctx context.Context, lbID string) (*govultr.LoadBalancer, error) {
	vlb, _, err := l.client.LoadBalancer.Get(ctx, lbID) //nolint:bodyclose
	if err != nil {
		return nil, errLbNotFound
	}

	return vlb, nil
}

func (l *loadbalancers) getVultrLB(ctx context.Context, service *v1.Service) (*govultr.LoadBalancer, error) {
	if id, ok := service.Annotations[annoVultrLoadBalancerID]; ok {
		lb, err := l.lbByID(ctx, id)
		if err != nil {
			return nil, err
		}
		return lb, nil
	}

	defaultLBName := getDefaultLBName(service)
	if lb, err := l.lbByName(ctx, defaultLBName); err != nil {
		lbName := l.GetLoadBalancerName(ctx, "", service)
		lb, err = l.lbByName(ctx, lbName)
		if err != nil {
			return nil, err
		}
		return lb, nil
	} else { //nolint
		return lb, nil
	}
}

func (l *loadbalancers) buildLoadBalancerRequest(service *v1.Service, nodes []*v1.Node) (*govultr.LoadBalancerReq, error) {
	stickySession, err := buildStickySession(service)
	if err != nil {
		return nil, err
	}

	healthCheck, err := buildHealthChecks(service)
	if err != nil {
		return nil, err
	}

	rules, err := buildForwardingRules(service)
	if err != nil {
		return nil, err
	}

	instances, err := buildInstanceList(nodes)
	if err != nil {
		return nil, err
	}

	timeout, err := getTimeout(service)
	if err != nil {
		return nil, err
	}

	var ssl *govultr.SSL
	if secretName, ok := service.Annotations[annoVultrLBSSL]; ok {
		ssl, err = l.GetSSL(service, secretName)
		if err != nil {
			return nil, err
		}
		SecretWatcher.AddService(service, secretName)
	} else {
		ssl = nil
	}

	firewallRules, err := buildFirewallRules(service)
	if err != nil {
		return nil, err
	}
	vpc, err := getVPC(service)
	if err != nil {
		return nil, err
	}

	nodeC := 1

	if count, ok := service.Annotations[annoVultrNodeCount]; ok {
		nodeC, err = strconv.Atoi(count)
		if err != nil {
			return nil, err
		}

		if nodeC&1 == 0 {
			return nil, fmt.Errorf("%s must be odd", annoVultrNodeCount)
		}
	}

	name := l.GetLoadBalancerName(context.Background(), "", service)

	return &govultr.LoadBalancerReq{
		Label:              name,                                             // will always be set
		Instances:          instances,                                        // will always be set
		HealthCheck:        healthCheck,                                      // will always be set
		StickySessions:     stickySession,                                    // need to check
		ForwardingRules:    rules,                                            // all always be set
		SSL:                ssl,                                              // will always be set
		SSLRedirect:        govultr.BoolToBoolPtr(getSSLRedirect(service)),   // need to check
		HTTP2:              govultr.BoolToBoolPtr(getHTTP2(service)),         // need to check
		HTTP3:              govultr.BoolToBoolPtr(getHTTP3(service)),         // need to check
		ProxyProtocol:      govultr.BoolToBoolPtr(getProxyProtocol(service)), // need to check
		BalancingAlgorithm: getAlgorithm(service),                            // will always be set
		FirewallRules:      firewallRules,                                    // need to check
		Timeout:            timeout,                                          // need to check
		VPC:                govultr.StringToStringPtr(vpc),                   // need to check
		Nodes:              nodeC,                                            // need to check
	}, nil
}

// getAlgorithm returns the algorithm to be used for load balancer service
// defaults to round_robin if no algorithm is provided.
func getAlgorithm(service *v1.Service) string {
	algo := service.Annotations[annoVultrAlgorithm]
	if algo == "least_connections" {
		return "leastconn"
	}

	return "roundrobin"
}

// getSSLRedirect returns if traffic should be redirected to https
// default to false if not specified
func getSSLRedirect(service *v1.Service) bool {
	redirect, ok := service.Annotations[annoVultrSSLRedirect]
	if !ok {
		return false
	}

	redirectBool, err := strconv.ParseBool(redirect)
	if err != nil {
		return false
	}

	return redirectBool
}

func buildStickySession(service *v1.Service) (*govultr.StickySessions, error) {
	enabled := getStickySessionEnabled(service)

	if enabled == "off" {
		return &govultr.StickySessions{
			CookieName: "",
		}, nil
	}

	cookieName, err := getCookieName(service)
	if err != nil {
		return nil, err
	}

	return &govultr.StickySessions{
		CookieName: cookieName,
	}, nil
}

// getStickySessionEnabled returns whether or not sticky sessions should be enabled
// default is off
func getStickySessionEnabled(service *v1.Service) string {
	enabled, ok := service.Annotations[annoVultrStickySessionEnabled]
	if !ok {
		return "off"
	}

	if enabled == "off" {
		return "off"
	} else if enabled == "on" {
		return "on"
	}

	return "off"
}

// getCookieName returns the cookie name for loadbalancer sticky sessions
func getCookieName(service *v1.Service) (string, error) {
	name, ok := service.Annotations[annoVultrStickySessionCookieName]
	if !ok || name == "" {
		return "", fmt.Errorf("sticky session cookie name name not supplied but is required")
	}

	return name, nil
}

func buildHealthChecks(service *v1.Service) (*govultr.HealthCheck, error) {
	healthCheckProtocol, err := getHealthCheckProtocol(service)
	if err != nil {
		return nil, err
	}

	port, err := getHealthCheckPort(service)
	if err != nil {
		return nil, err
	}

	path := getHealthCheckPath(service)

	interval, err := getHealthCheckInterval(service)
	if err != nil {
		return nil, err
	}

	response, err := getHealthCheckResponse(service)
	if err != nil {
		return nil, err
	}

	unhealthy, err := getHealthCheckUnhealthy(service)
	if err != nil {
		return nil, err
	}

	healthy, err := getHealthCheckHealthy(service)
	if err != nil {
		return nil, err
	}

	return &govultr.HealthCheck{
		Protocol:           healthCheckProtocol,
		Port:               port,
		Path:               path,
		CheckInterval:      interval,
		ResponseTimeout:    response,
		UnhealthyThreshold: unhealthy,
		HealthyThreshold:   healthy,
	}, nil
}

// getHealthCheckProtocol returns the protocol for a health check
// default is TCP
func getHealthCheckProtocol(service *v1.Service) (string, error) {
	protocol := service.Annotations[annoVultrHealthCheckProtocol]

	// add in https
	if protocol == "" {
		if getHealthCheckPath(service) != "" {
			return protocolHTTP, nil
		}
		return protocolTCP, nil
	}

	if protocol != protocolHTTP && protocol != protocolTCP {
		return "", fmt.Errorf("invalid protocol : %s given in the anootation : %s", protocol, annoVultrHealthCheckProtocol)
	}

	return protocol, nil
}

// getHealthCheckPath returns the path for a health check
func getHealthCheckPath(service *v1.Service) string {
	path, ok := service.Annotations[annoVultrHealthCheckPath]
	if !ok {
		return ""
	}

	return path
}

func getHealthCheckPort(service *v1.Service) (int, error) {
	port, ok := service.Annotations[annoVultrHealthCheckPort]
	if !ok {
		return int(service.Spec.Ports[0].NodePort), nil
	}

	portInt, err := strconv.Atoi(port)
	if err != nil {
		return 0, err
	}

	for _, v := range service.Spec.Ports {
		if int(v.Port) == portInt {
			return int(v.Port), nil
		}
		// The provided port does not exist
		return 0, fmt.Errorf("provided health check port %d does not exist for service %s/%s", portInt, service.Namespace, service.Name) //nolint
	}

	// need to default a return here
	return 0, nil
}

func getHealthCheckInterval(service *v1.Service) (int, error) {
	interval, ok := service.Annotations[annoVultrHealthCheckInterval]
	if !ok {
		return healthCheckInterval, nil
	}

	intervalInt, err := strconv.Atoi(interval)
	if err != nil {
		return 0, fmt.Errorf("failed to retrieve health check interval %s - %s", annoVultrHealthCheckInterval, err)
	}

	return intervalInt, err
}

func getHealthCheckResponse(service *v1.Service) (int, error) {
	response, ok := service.Annotations[annoVultrHealthCheckResponseTimeout]
	if !ok {
		return healthCheckResponse, nil
	}

	responseInt, err := strconv.Atoi(response)
	if err != nil {
		return 0, fmt.Errorf("failed to retrieve health check response timeout %s - %s", annoVultrHealthCheckResponseTimeout, err)
	}

	return responseInt, err
}

func getHealthCheckUnhealthy(service *v1.Service) (int, error) {
	unhealthy, ok := service.Annotations[annoVultrHealthCheckUnhealthyThreshold]
	if !ok {
		return healthCheckUnhealthy, nil
	}

	unhealthyInt, err := strconv.Atoi(unhealthy)
	if err != nil {
		return 0, fmt.Errorf("failed to retrieve health check unhealthy treshold %s - %s", annoVultrHealthCheckUnhealthyThreshold, err)
	}

	return unhealthyInt, err
}

func getHealthCheckHealthy(service *v1.Service) (int, error) {
	healthy, ok := service.Annotations[annoVultrHealthCheckHealthyThreshold]
	if !ok {
		return healthCheckHealthy, nil
	}

	healthyInt, err := strconv.Atoi(healthy)
	if err != nil {
		return 0, fmt.Errorf("failed to retrieve health check healthy treshold %s - %s", annoVultrHealthCheckHealthyThreshold, err)
	}

	return healthyInt, err
}

// buildInstanceList create list of nodes to be attached to a load balancer
func buildInstanceList(nodes []*v1.Node) ([]string, error) {
	var list []string

	for _, node := range nodes {
		instanceID, err := vultrIDFromProviderID(node.Spec.ProviderID)
		if err != nil {
			return nil, fmt.Errorf("error getting the provider ID %s : %s", node.Spec.ProviderID, err)
		}

		list = append(list, instanceID)
	}

	return list, nil
}

func buildForwardingRules(service *v1.Service) ([]govultr.ForwardingRule, error) {
	var rules []govultr.ForwardingRule

	defaultProtocol := getLBProtocol(service)

	httpsPorts, err := getHTTPSPorts(service)
	if err != nil {
		return nil, err
	}

	for _, port := range service.Spec.Ports {
		// default the port
		frontendProtocol := defaultProtocol
		backendProtocol := getBackendProtocol(service)

		if httpsPorts[port.Port] {
			if getSSLPassthrough(service) {
				frontendProtocol = protocolTCP
			} else {
				frontendProtocol = protocolHTTPS
			}
		}

		// Check frontend/backend port combinations (listed below what is acceptable)
		// frontend = tcp: backend must be tcp
		// frontend = https: backend can be http(s)
		// frontend = http: backend can be http(s)
		switch frontendProtocol {
		case "tcp":
			if backendProtocol != "tcp" {
				klog.Infof("When frontend proto is tcp, backend default is tcp, %q is out of supported range, setting backend to tcp", backendProtocol)
				backendProtocol = "tcp"
			}
		case "http":
			if backendProtocol != "http" && backendProtocol != "https" {
				klog.Infof("When frontend proto is http, backend default is http, %q is out of supported range, setting backend to http", backendProtocol)
				backendProtocol = "http" // http is default
			}
		case "https":
			if backendProtocol != "http" && backendProtocol != "https" {
				klog.Infof("When frontend proto is https, backend default is https, %q is out of supported range, setting backend to https", backendProtocol)
				backendProtocol = "https" // https is default
			}
		}

		// unset backend should be same as frontend
		if backendProtocol == "" {
			backendProtocol = frontendProtocol
		}
		klog.Infof("Frontend: %q, Backend: %q", frontendProtocol, backendProtocol)

		rule, err := buildForwardingRule(&port, frontendProtocol, backendProtocol) //nolint
		if err != nil {
			return nil, err
		}

		rules = append(rules, *rule)
	}

	return rules, nil
}

func buildForwardingRule(port *v1.ServicePort, protocol, backendProtocol string) (*govultr.ForwardingRule, error) {
	var rule govultr.ForwardingRule

	if port.Protocol == portProtocolUDP {
		return nil, fmt.Errorf("TCP protocol is only supported: received %s", port.Protocol)
	}

	rule.FrontendProtocol = protocol
	rule.BackendProtocol = backendProtocol

	klog.V(logLevel).Infof("Rule: %+v\n", rule) //nolint

	rule.FrontendPort = int(port.Port)
	rule.BackendPort = int(port.NodePort)

	return &rule, nil
}

func getLBProtocol(service *v1.Service) string {
	protocol, ok := service.Annotations[annoVultrLBProtocol]
	if !ok {
		return protocolTCP
	}

	return protocol
}

func getHTTPSPorts(service *v1.Service) (map[int32]bool, error) {
	ports, ok := service.Annotations[annoVultrLbHTTPSPorts]
	if !ok {
		return nil, nil
	}

	portStrings := strings.Split(ports, ",")
	portInt := map[int32]bool{}

	for _, port := range portStrings {
		p, err := strconv.Atoi(port)
		if err != nil {
			return nil, err
		}
		portInt[int32(p)] = true //nolint could cause integer overflow if p > 32-bits
	}
	return portInt, nil
}

func (l *loadbalancers) GetSSL(service *v1.Service, secretName string) (*govultr.SSL, error) {
	if err := l.GetKubeClient(); err != nil {
		return nil, err
	}

	secret, err := l.kubeClient.CoreV1().Secrets(service.Namespace).Get(context.Background(), secretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	cert := string(secret.Data[v1.TLSCertKey])
	cert = strings.TrimSpace(cert)

	key := string(secret.Data[v1.TLSPrivateKeyKey])
	key = strings.TrimSpace(key)

	ssl := govultr.SSL{
		PrivateKey:  key,
		Certificate: cert,
	}
	return &ssl, nil
}

func (l *loadbalancers) GetKubeClient() error {
	if l.kubeClient != nil {
		return nil
	}

	var (
		kubeConfig *rest.Config
		err        error
		config     string
	)

	// If no kubeconfig was passed in or set then we want to default to an empty string
	// This will have `clientcmd.BuildConfigFromFlags` default to `restclient.InClusterConfig()` which was existing behavior
	if Options.KubeconfigFlag == nil || Options.KubeconfigFlag.Value.String() == "" {
		config = ""
	} else {
		config = Options.KubeconfigFlag.Value.String()
	}

	kubeConfig, err = clientcmd.BuildConfigFromFlags("", config)
	if err != nil {
		return err
	}

	l.kubeClient, err = kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}

	return nil
}

func getSSLPassthrough(service *v1.Service) bool {
	passThrough, ok := service.Annotations[annoVultrLBSSLPassthrough]
	if !ok {
		return false
	}

	pass, err := strconv.ParseBool(passThrough)
	if err != nil {
		return false
	}
	return pass
}

func getProxyProtocol(service *v1.Service) bool {
	proxy, ok := service.Annotations[annoVultrProxyProtocol]
	if !ok {
		return false
	}

	pass, err := strconv.ParseBool(proxy)
	if err != nil {
		return false
	}

	return pass
}

func getHTTP2(service *v1.Service) bool {
	http2, ok := service.Annotations[annoVultrLBHTTP2]
	if !ok {
		return false
	}

	protocolHTTP2, err := strconv.ParseBool(http2)
	if err != nil {
		return false
	}

	return protocolHTTP2
}

func getHTTP3(service *v1.Service) bool {
	http3, ok := service.Annotations[annoVultrLBHTTP3]
	if !ok {
		return false
	}

	protocolHTTP3, err := strconv.ParseBool(http3)
	if err != nil {
		return false
	}

	return protocolHTTP3
}

func getTimeout(service *v1.Service) (int, error) {
	lbtimeout, ok := service.Annotations[annoVultrLBTimeout]
	if !ok {
		return defaultLBTimeout, nil
	}

	timeout, err := strconv.Atoi(lbtimeout)
	if err != nil {
		return 0, fmt.Errorf("invalid timeout value: %v", err)
	}
	return timeout, nil
}

func buildFirewallRules(service *v1.Service) ([]govultr.LBFirewallRule, error) {
	lbFWRules := []govultr.LBFirewallRule{}
	fwRules := getFirewallRules(service)
	if fwRules == "" {
		return lbFWRules, nil
	}

	for _, v := range strings.Split(fwRules, ";") {
		fwRule := govultr.LBFirewallRule{}

		rules := strings.Split(v, ",")
		if len(rules) != 2 { //nolint
			return nil, fmt.Errorf("loadbalancer fw rules : %s invalid configuration", rules)
		}

		source := rules[0]
		ipType := "v4"
		if source != "cloudflare" {
			ip, _, err := net.ParseCIDR(source)
			if err != nil {
				return nil, fmt.Errorf("loadbalancer fw rules : source %s is invalid", source)
			}

			if ip.To4() == nil {
				ipType = "v6"
			}
		}

		port, err := strconv.Atoi(rules[1])
		if err != nil {
			return nil, fmt.Errorf("loadbalancer fw rules : port %d is invalid", port)
		}

		fwRule.Source = source
		fwRule.IPType = ipType
		fwRule.Port = port
		lbFWRules = append(lbFWRules, fwRule)
	}
	return lbFWRules, nil
}

func getFirewallRules(service *v1.Service) string {
	fwRules, ok := service.Annotations[annoVultrFirewallRules]
	if !ok {
		return ""
	}

	return fwRules
}

func getVPC(service *v1.Service) (string, error) {
	var vpc string
	pn, pnOk := service.Annotations[annoVultrPrivateNetwork]
	v, vpcOk := service.Annotations[annoVultrVPC]

	if vpcOk && pnOk {
		return "", fmt.Errorf("can not use private_network and vpc annotations. Please use VPC as private network is deprecated")
	} else if pnOk {
		vpc = pn
	} else if vpcOk {
		vpc = v
	} else {
		return "", nil
	}

	if strings.EqualFold(vpc, "false") {
		return "", nil
	}

	meta := metadata.NewClient()
	m, err := meta.Metadata()
	if err != nil {
		return "", fmt.Errorf("error getting metadata for private_network: %v", err.Error())
	}

	pnID := ""
	for _, v := range m.Interfaces { //nolint
		if v.NetworkV2ID != "" {
			pnID = v.NetworkV2ID
			break
		}
	}

	return pnID, nil
}

func getBackendProtocol(service *v1.Service) string {
	proto, ok := service.Annotations[annoVultrLBBackendProtocol]
	if !ok {
		return ""
	}

	switch proto {
	case "http":
		return protocolHTTP
	case "https":
		return protocolHTTPS
	case "tcp":
		return protocolTCP
	default:
		return ""
	}
}

// checkEnabledIPv6 checks whether or not IPv6 is requested on the resource
func checkEnabledIPv6(service *v1.Service) bool {
	if family := service.Spec.IPFamilies; len(family) >= 1 {
		for _, fam := range family {
			if fam == "IPv6" {
				return true
			}
		}
	}

	if service.Spec.IPFamilyPolicy != nil {
		policy := *service.Spec.IPFamilyPolicy
		if policy == v1.IPFamilyPolicyPreferDualStack || policy == v1.IPFamilyPolicyRequireDualStack {
			return true
		}
	}

	return false
}
