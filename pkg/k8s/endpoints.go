// Copyright 2018-2020 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8s

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"

	"github.com/cilium/cilium/pkg/ip"
	"github.com/cilium/cilium/pkg/k8s/types"
	"github.com/cilium/cilium/pkg/k8s/version"
	"github.com/cilium/cilium/pkg/loadbalancer"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/service"

	v1 "k8s.io/api/core/v1"
	"k8s.io/api/discovery/v1beta1"
)

// Endpoints is an abstraction for the Kubernetes endpoints object. Endpoints
// consists of a set of backend IPs in combination with a set of ports and
// protocols. The name of the backend ports must match the names of the
// frontend ports of the corresponding service.
// +k8s:deepcopy-gen=true
type Endpoints struct {
	// Backends is a map containing all backend IPs and ports. The key to
	// the map is the backend IP in string form. The value defines the list
	// of ports for that backend IP, plus an additional optional node name.
	Backends map[string]*Backend
}

// Backend contains all ports and the node name of a given backend
// +k8s:deepcopy-gen=true
type Backend struct {
	Ports    service.PortConfiguration
	NodeName string
}

// DeepEquals returns true if both Backends are identical
func (b *Backend) DeepEquals(o *Backend) bool {
	switch {
	case (b == nil) != (o == nil):
		return false
	case (b == nil) && (o == nil):
		return true
	}

	return b.NodeName == o.NodeName && b.Ports.DeepEquals(o.Ports)
}

// String returns the string representation of an endpoints resource, with
// backends and ports sorted.
func (e *Endpoints) String() string {
	if e == nil {
		return ""
	}

	backends := []string{}
	for ip, be := range e.Backends {
		for _, port := range be.Ports {
			backends = append(backends, fmt.Sprintf("%s/%s", net.JoinHostPort(ip, strconv.Itoa(int(port.Port))), port.Protocol))
		}
	}

	sort.Strings(backends)

	return strings.Join(backends, ",")
}

// newEndpoints returns a new Endpoints
func newEndpoints() *Endpoints {
	return &Endpoints{
		Backends: map[string]*Backend{},
	}
}

// DeepEquals returns true if both endpoints are deep equal.
func (e *Endpoints) DeepEquals(o *Endpoints) bool {
	switch {
	case (e == nil) != (o == nil):
		return false
	case (e == nil) && (o == nil):
		return true
	}

	if len(e.Backends) != len(o.Backends) {
		return false
	}

	for ip1, backend1 := range e.Backends {
		backend2, ok := o.Backends[ip1]
		if !ok {
			return false
		}

		if !backend1.DeepEquals(backend2) {
			return false
		}
	}

	return true
}

// CIDRPrefixes returns the endpoint's backends as a slice of IPNets.
func (e *Endpoints) CIDRPrefixes() ([]*net.IPNet, error) {
	prefixes := make([]string, len(e.Backends))
	index := 0
	for ip := range e.Backends {
		prefixes[index] = ip
		index++
	}

	valid, invalid := ip.ParseCIDRs(prefixes)
	if len(invalid) > 0 {
		return nil, fmt.Errorf("invalid IPs specified as backends: %+v", invalid)
	}

	return valid, nil
}

// ParseEndpointsID parses a Kubernetes endpoints and returns the ServiceID
func ParseEndpointsID(svc *types.Endpoints) ServiceID {
	return ServiceID{
		Name:      svc.ObjectMeta.Name,
		Namespace: svc.ObjectMeta.Namespace,
	}
}

// ParseEndpoints parses a Kubernetes Endpoints resource
func ParseEndpoints(ep *types.Endpoints) (ServiceID, *Endpoints) {
	endpoints := newEndpoints()

	for _, sub := range ep.Subsets {
		for _, addr := range sub.Addresses {
			backend, ok := endpoints.Backends[addr.IP]
			if !ok {
				backend = &Backend{Ports: service.PortConfiguration{}}
				endpoints.Backends[addr.IP] = backend
			}

			if addr.NodeName != nil {
				backend.NodeName = *addr.NodeName
			}

			for _, port := range sub.Ports {
				lbPort := loadbalancer.NewL4Addr(loadbalancer.L4Type(port.Protocol), uint16(port.Port))
				backend.Ports[port.Name] = lbPort
			}
		}
	}

	return ParseEndpointsID(ep), endpoints
}

// ParseEndpointSliceID parses a Kubernetes endpoints slice and returns the
// ServiceID
func ParseEndpointSliceID(svc *types.EndpointSlice) ServiceID {
	return ServiceID{
		Name:      svc.ObjectMeta.GetLabels()[v1beta1.LabelServiceName],
		Namespace: svc.ObjectMeta.Namespace,
	}
}

// ParseEndpointSlice parses a Kubernetes Endpoints resource
func ParseEndpointSlice(ep *types.EndpointSlice) (ServiceID, *Endpoints) {
	endpoints := newEndpoints()

	for _, sub := range ep.Endpoints {
		// ready indicates that this endpoint is prepared to receive traffic,
		// according to whatever system is managing the endpoint. A nil value
		// indicates an unknown state. In most cases consumers should interpret this
		// unknown state as ready.
		// More info: vendor/k8s.io/api/discovery/v1beta1/types.go:114
		if sub.Conditions.Ready != nil && !*sub.Conditions.Ready {
			continue
		}
		for _, addr := range sub.Addresses {
			backend, ok := endpoints.Backends[addr]
			if !ok {
				backend = &Backend{Ports: service.PortConfiguration{}}
				endpoints.Backends[addr] = backend
				if nodeName, ok := sub.Topology["kubernetes.io/hostname"]; ok {
					backend.NodeName = nodeName
				}
			}

			for _, port := range ep.Ports {
				name, lbPort := parseEndpointPort(port)
				if lbPort != nil {
					backend.Ports[name] = lbPort
				}
			}
		}
	}

	return ParseEndpointSliceID(ep), endpoints
}

// parseEndpointPort returns the port name and the port parsed as a L4Addr from
// the given port.
func parseEndpointPort(port v1beta1.EndpointPort) (string, *loadbalancer.L4Addr) {
	proto := loadbalancer.TCP
	if port.Protocol != nil {
		switch *port.Protocol {
		case v1.ProtocolTCP:
			proto = loadbalancer.TCP
		case v1.ProtocolUDP:
			proto = loadbalancer.UDP
		default:
			return "", nil
		}
	}
	if port.Port == nil {
		return "", nil
	}
	var name string
	if port.Name != nil {
		name = *port.Name
	}
	lbPort := loadbalancer.NewL4Addr(proto, uint16(*port.Port))
	return name, lbPort
}

// externalEndpoints is the collection of external endpoints in all remote
// clusters. The map key is the name of the remote cluster.
type externalEndpoints struct {
	endpoints map[string]*Endpoints
}

// newExternalEndpoints returns a new ExternalEndpoints
func newExternalEndpoints() externalEndpoints {
	return externalEndpoints{
		endpoints: map[string]*Endpoints{},
	}
}

// SupportsEndpointSlice returns true if cilium-operator or cilium-agent should
// watch and process endpoint slices.
func SupportsEndpointSlice() bool {
	return version.Capabilities().EndpointSlice && option.Config.K8sEnableK8sEndpointSlice
}

// HasEndpointSlice returns true if the hasEndpointSlices is closed before the
// controller has been synchronized with k8s.
func HasEndpointSlice(hasEndpointSlices chan struct{}, controller cache.Controller) bool {
	endpointSliceSynced := make(chan struct{})
	go func() {
		cache.WaitForCacheSync(wait.NeverStop, controller.HasSynced)
		close(endpointSliceSynced)
	}()

	// Check if K8s has a single endpointslice endpoint. By default, k8s has
	// always the kubernetes-apiserver endpoint. If the endpointSlice are synced
	// but we haven't received any endpoint slice then it means k8s is not
	// running with k8s endpoint slice enabled.
	select {
	case <-endpointSliceSynced:
		select {
		// In case both select cases are ready to be selected we will recheck if
		// hasEndpointSlices was closed.
		case <-hasEndpointSlices:
			return true
		default:
		}
	case <-hasEndpointSlices:
		return true
	}
	return false
}
