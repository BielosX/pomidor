/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ListenerSpec struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Service  string `json:"service,omitempty"`
}

type SecurityGroupIngressSpec struct {
	Protocol string `json:"protocol"`
	FromPort int    `json:"fromPort"`
	ToPort   int    `json:"toPort"`
	CidrIp   string `json:"cidrIp"`
}

// NetworkLoadBalancerSpec defines the desired state of NetworkLoadBalancer
type NetworkLoadBalancerSpec struct {
	Internal             bool                       `json:"internal,omitempty"`
	Listeners            []ListenerSpec             `json:"listeners"`
	SecurityGroupIngress []SecurityGroupIngressSpec `json:"securityGroupIngress"`
}

// NetworkLoadBalancerStatus defines the observed state of NetworkLoadBalancer
type NetworkLoadBalancerStatus struct {
	Arn     string `json:"arn"`
	DnsName string `json:"dnsName"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// NetworkLoadBalancer is the Schema for the networkloadbalancers API
type NetworkLoadBalancer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NetworkLoadBalancerSpec   `json:"spec,omitempty"`
	Status NetworkLoadBalancerStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// NetworkLoadBalancerList contains a list of NetworkLoadBalancer
type NetworkLoadBalancerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetworkLoadBalancer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NetworkLoadBalancer{}, &NetworkLoadBalancerList{})
}
