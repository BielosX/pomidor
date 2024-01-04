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

package controller

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	types2 "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"k8s.io/client-go/tools/record"

	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	awsv1 "pomidor/pomidor/api/v1"
)

// NetworkLoadBalancerReconciler reconciles a NetworkLoadBalancer object
type NetworkLoadBalancerReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	ElbClient      *elbv2.Client
	Ec2Client      *ec2.Client
	PrivateSubnets []string
	PublicSubnets  []string
	ClusterName    string
	VpcId          string
}

//+kubebuilder:rbac:groups=aws.pomidor,resources=networkloadbalancers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=aws.pomidor,resources=networkloadbalancers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=aws.pomidor,resources=networkloadbalancers/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=services,verbs=get
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *NetworkLoadBalancerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	namespacedName := req.NamespacedName
	var networkLoadBalancer awsv1.NetworkLoadBalancer
	err := r.Get(ctx, namespacedName, &networkLoadBalancer)
	if err != nil {
		logger.Error(err, "Unable to fetch NetworkLoadBalancer")
		return ctrl.Result{}, err
	}

	if networkLoadBalancer.Status.Arn == nil {
		lbName := fmt.Sprintf("%s-%s-%s", r.ClusterName, namespacedName.Namespace, namespacedName.Name)
		isInternal := networkLoadBalancer.Spec.Internal
		var lbScheme types.LoadBalancerSchemeEnum
		var subnets []string
		if !isInternal {
			lbScheme = types.LoadBalancerSchemeEnumInternetFacing
			subnets = r.PublicSubnets
		} else {
			lbScheme = types.LoadBalancerSchemeEnumInternal
			subnets = r.PrivateSubnets
		}
		sgDescription := fmt.Sprintf("%s %s NLB Security Group", namespacedName.Namespace, namespacedName.Name)
		sgName := fmt.Sprintf("%s-%s-%s-nlb-sg", r.ClusterName, namespacedName.Namespace, namespacedName.Name)
		clusterTag := "Cluster"
		sgOut, err := r.Ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
			Description: &sgDescription,
			GroupName:   &sgName,
			VpcId:       &r.VpcId,
			TagSpecifications: []types2.TagSpecification{
				{
					ResourceType: types2.ResourceTypeSecurityGroup,
					Tags: []types2.Tag{
						{
							Key:   &clusterTag,
							Value: &r.ClusterName,
						},
					},
				},
			},
		})
		if err != nil {
			logger.Error(err, "Unable to create SecurityGroup")
			r.Recorder.Event(&networkLoadBalancer, "Error", "Failed", "Unable to create Security Group")
			return ctrl.Result{}, nil
		}
		lbOut, err := r.ElbClient.CreateLoadBalancer(ctx, &elbv2.CreateLoadBalancerInput{
			Name:           &lbName,
			Type:           types.LoadBalancerTypeEnumNetwork,
			Scheme:         lbScheme,
			Subnets:        subnets,
			SecurityGroups: []string{*sgOut.GroupId},
		})
		if err != nil {
			logger.Error(err, "Unable to create Network Load Balancer")
			r.Recorder.Event(&networkLoadBalancer, "Error", "Failed", "Unable to create NLB")
			return ctrl.Result{}, nil
		}
		networkLoadBalancer.Status.Arn = lbOut.LoadBalancers[0].LoadBalancerArn
		networkLoadBalancer.Status.DnsName = lbOut.LoadBalancers[0].DNSName
		err = r.Status().Update(ctx, &networkLoadBalancer)
		if err != nil {
			logger.Error(err, "Unable to update status")
			return ctrl.Result{}, err
		}
	} else {

	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NetworkLoadBalancerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&awsv1.NetworkLoadBalancer{}).
		Complete(r)
}
