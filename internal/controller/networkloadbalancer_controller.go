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
	"errors"
	"fmt"
	aws2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elb2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/smithy-go"
	"github.com/thoas/go-funk"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	awsv1 "pomidor/pomidor/api/v1"
	"pomidor/pomidor/internal/aws"
	"pomidor/pomidor/internal/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"strings"
	"time"
)

const finalizerName = "networkloadbalancers.aws.pomidor/finalizer"
const clusterTag = "Cluster"
const managedByTag = "ManagedBy"
const pomidor = "pomidor"
const namespaceTag = "k8s/namespace"
const resourceTag = "k8s/resource"
const targetGroupNotAttachedToAsg = "Trying to remove Target Groups that are not part of the group"

// NetworkLoadBalancerReconciler reconciles a NetworkLoadBalancer object
type NetworkLoadBalancerReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	ElbClient      *elbv2.Client
	Ec2Client      *ec2.Client
	AsgClient      *autoscaling.Client
	PrivateSubnets []string
	PublicSubnets  []string
	ClusterName    string
	VpcId          string
	ClusterSgId    string
	NodesAsgNames  []string
}

type Tag struct {
	Key   *string
	Value *string
}

func (tag *Tag) asElbTag() elb2types.Tag {
	return elb2types.Tag{
		Key:   tag.Key,
		Value: tag.Value,
	}
}

func (tag *Tag) asEc2Tag() ec2types.Tag {
	return ec2types.Tag{
		Key:   tag.Key,
		Value: tag.Value,
	}
}

//+kubebuilder:rbac:groups=aws.pomidor,resources=networkloadbalancers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=aws.pomidor,resources=networkloadbalancers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=aws.pomidor,resources=networkloadbalancers/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *NetworkLoadBalancerReconciler) deleteListeners(ctx context.Context, nlbArn *string) error {
	logger := log.FromContext(ctx)
	out, err := r.ElbClient.DescribeListeners(ctx, &elbv2.DescribeListenersInput{
		LoadBalancerArn: nlbArn,
	})
	if err != nil {
		var elbErr *elb2types.LoadBalancerNotFoundException
		if errors.As(err, &elbErr) {
			return nil
		} else {
			logger.Error(err, "Unable to describe NLB listeners")
			return err
		}
	}
	for _, listener := range out.Listeners {
		_, err := r.ElbClient.DeleteListener(ctx, &elbv2.DeleteListenerInput{
			ListenerArn: listener.ListenerArn,
		})
		if err != nil {
			logger.Error(err, "Unable to delete listener")
			return err
		}
	}
	return nil
}

func (r *NetworkLoadBalancerReconciler) getTargetGroupsArn(ctx context.Context,
	nlbArn *string) ([]string, error) {
	logger := log.FromContext(ctx)
	var result []string
	paginator := elbv2.NewDescribeTargetGroupsPaginator(r.ElbClient, &elbv2.DescribeTargetGroupsInput{
		LoadBalancerArn: nlbArn,
	})
	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		var loadBalancerNotFound *elb2types.LoadBalancerNotFoundException
		if err != nil {
			if errors.As(err, &loadBalancerNotFound) {
				return nil, nil
			} else {
				logger.Error(err, "Unable to describe target groups")
				return nil, err
			}
		}
		arnList := funk.Map(out.TargetGroups, func(group elb2types.TargetGroup) string {
			return *group.TargetGroupArn
		}).([]string)
		result = append(result, arnList...)
	}
	return result, nil
}

func (r *NetworkLoadBalancerReconciler) deleteExternalResources(ctx context.Context,
	networkLoadBalancer *awsv1.NetworkLoadBalancer) error {
	logger := log.FromContext(ctx)
	nlbArn := networkLoadBalancer.Status.Arn
	securityGroupIds := networkLoadBalancer.Status.SecurityGroupIds
	clusterSgRuleId := networkLoadBalancer.Status.ClusterSgRuleId
	_, err := r.Ec2Client.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
		GroupId:              &r.ClusterSgId,
		SecurityGroupRuleIds: []string{*clusterSgRuleId},
	})
	if err != nil {
		var apiErr smithy.APIError
		errors.As(err, &apiErr)
		if "InvalidSecurityGroupRuleId.NotFound" == apiErr.ErrorCode() {
			logger.Info("Cluster SG Rule probably already deleted")
		} else {
			logger.Error(err, "Unable to remove Cluster SG rule")
			return err
		}
	}
	tgOut, err := r.getTargetGroupsArn(ctx, nlbArn)
	if err != nil {
		logger.Error(err, "Unable to fetch target group ARNs")
		return err
	}
	for _, asg := range r.NodesAsgNames {
		for _, targetGroupArn := range tgOut {
			_, err := r.AsgClient.DetachTrafficSources(ctx, &autoscaling.DetachTrafficSourcesInput{
				AutoScalingGroupName: &asg,
				TrafficSources: []asgtypes.TrafficSourceIdentifier{
					{
						Identifier: &targetGroupArn,
					},
				},
			})
			if err != nil {
				var apiErr smithy.APIError
				errors.As(err, &apiErr)
				if strings.HasPrefix(apiErr.ErrorMessage(), targetGroupNotAttachedToAsg) {
					logger.Info(fmt.Sprintf("Target group %s probably already detached from %s", targetGroupArn, asg))
				} else {
					logger.Error(err, fmt.Sprintf("Unable to detach target group %s", targetGroupArn))
					return err
				}
			}
		}
	}
	err = r.deleteListeners(ctx, nlbArn)
	if err != nil {
		logger.Error(err, "Unable to delete NLB listeners")
		return err
	}
	for _, tgArn := range tgOut {
		_, err := r.ElbClient.DeleteTargetGroup(ctx, &elbv2.DeleteTargetGroupInput{
			TargetGroupArn: &tgArn,
		})
		if err != nil {
			logger.Error(err, "Unable to delete target group")
			return err
		}
	}
	_, err = r.ElbClient.DeleteLoadBalancer(ctx, &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: nlbArn,
	})
	if err != nil {
		logger.Error(err, fmt.Sprintf("Unable to delete NLB %s", *nlbArn))
		return err
	}
	logger.Info(fmt.Sprintf("Waiting for Load Balancer %s to be destroyed", *nlbArn))
	waiter := elbv2.NewLoadBalancersDeletedWaiter(r.ElbClient)
	err = waiter.Wait(ctx, &elbv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []string{*nlbArn},
	}, time.Minute)
	if err != nil {
		logger.Error(err, "LoadBalancersDeletedWaiter failed")
		return err
	}
	for _, sg := range securityGroupIds {
		err := r.deleteSecurityGroupRules(ctx, &sg)
		if err != nil {
			logger.Error(err, "Unable to delete security group rules")
			return err
		}
		_, err = r.Ec2Client.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: &sg,
		})
		if err != nil {
			logger.Error(err, fmt.Sprintf("Unable to delete security group with id %s", sg))
			return err
		}
	}
	logger.Info("External resources deleted")
	return nil
}

func (r *NetworkLoadBalancerReconciler) createIngressRules(ctx context.Context,
	networkLoadBalancer *awsv1.NetworkLoadBalancer,
	securityGroupId *string) error {
	logger := log.FromContext(ctx)
	var ruleName string
	for _, ingress := range networkLoadBalancer.Spec.SecurityGroupIngress {
		fromPort := int32(ingress.FromPort)
		toPort := int32(ingress.FromPort)
		protocol := strings.ToLower(ingress.Protocol)
		ipPermission := ec2types.IpPermission{
			FromPort:   &fromPort,
			ToPort:     &toPort,
			IpProtocol: &protocol,
		}
		if ingress.CidrIp != nil {
			ipPermission.IpRanges = []ec2types.IpRange{
				{
					CidrIp: ingress.CidrIp,
				},
			}
			ruleName = fmt.Sprintf("%d-%d-%s-%s", fromPort, toPort, protocol, *ingress.CidrIp)
		}
		if ingress.SourceSecurityGroupId != nil {
			ipPermission.UserIdGroupPairs = []ec2types.UserIdGroupPair{
				{
					GroupId: ingress.SourceSecurityGroupId,
					VpcId:   &r.VpcId,
				},
			}
			ruleName = fmt.Sprintf("ingress-%d-%d-%s-%s", fromPort, toPort, protocol, *ingress.SourceSecurityGroupId)
		}
		_, err := aws.AuthorizeSecurityGroup(r.Ec2Client, ctx, securityGroupId, false, ruleName, []ec2types.IpPermission{ipPermission})
		if err != nil {
			logger.Error(err, "Unable to create security group ingress rule")
			return err
		}
	}
	return nil
}

func (r *NetworkLoadBalancerReconciler) addClusterSecurityGroupIngressRule(ctx context.Context,
	nlbSecurityGroupId *string) (*string, error) {
	logger := log.FromContext(ctx)
	fromPort := int32(0)
	toPort := int32(0)
	protocol := "-1"
	ipPermission := ec2types.IpPermission{
		IpProtocol: &protocol,
		FromPort:   &fromPort,
		ToPort:     &toPort,
		UserIdGroupPairs: []ec2types.UserIdGroupPair{
			{
				GroupId: nlbSecurityGroupId,
				VpcId:   &r.VpcId,
			},
		},
	}
	ruleName := fmt.Sprintf("ingress-%d-%d-%s-%s", fromPort, toPort, protocol, *nlbSecurityGroupId)
	logger.Info(fmt.Sprintf("Creating cluster security group %s ingress rule %s", r.ClusterSgId, ruleName))
	ruleId, err := aws.AuthorizeSecurityGroup(r.Ec2Client, ctx, &r.ClusterSgId, false, ruleName, []ec2types.IpPermission{ipPermission})
	if err != nil {
		logger.Error(err, "Unable to add Cluster SG Rule")
		return nil, err
	}
	logger.Info(fmt.Sprintf("Cluster SG %s ingress rule %s created", r.ClusterSgId, *ruleId))
	return ruleId, nil
}

func (r *NetworkLoadBalancerReconciler) createEgressRule(ctx context.Context,
	nlbSecurityGroupId *string) error {
	logger := log.FromContext(ctx)
	fromPort := int32(0)
	toPort := int32(0)
	protocol := "-1"
	ipPermission := ec2types.IpPermission{
		IpProtocol: &protocol,
		FromPort:   &fromPort,
		ToPort:     &toPort,
		UserIdGroupPairs: []ec2types.UserIdGroupPair{
			{
				GroupId: &r.ClusterSgId,
				VpcId:   &r.VpcId,
			},
		},
	}
	ruleName := fmt.Sprintf("egress-%d-%d-%s-%s", fromPort, toPort, protocol, r.ClusterSgId)
	_, err := aws.AuthorizeSecurityGroup(r.Ec2Client, ctx, nlbSecurityGroupId, true, ruleName, []ec2types.IpPermission{ipPermission})
	if err != nil {
		logger.Error(err, "Unable to create security group egress rule")
		return err
	}
	return nil
}

func (r *NetworkLoadBalancerReconciler) deleteSecurityGroupRules(ctx context.Context,
	nlbSecurityGroupId *string) error {
	logger := log.FromContext(ctx)
	paginator := ec2.NewDescribeSecurityGroupRulesPaginator(r.Ec2Client, &ec2.DescribeSecurityGroupRulesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws2.String("group-id"),
				Values: []string{*nlbSecurityGroupId},
			},
		},
	})
	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			logger.Error(err, "Unable to describe security group rules")
			return err
		}
		for _, rule := range out.SecurityGroupRules {
			if *rule.IsEgress {
				_, err := r.Ec2Client.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
					GroupId:              nlbSecurityGroupId,
					SecurityGroupRuleIds: []string{*rule.SecurityGroupRuleId},
				})
				if err != nil {
					var apiErr smithy.APIError
					errors.As(err, &apiErr)
					if "InvalidSecurityGroupRuleId.NotFound" == apiErr.ErrorCode() {
						logger.Info(fmt.Sprintf("SG Rule %s probably already deleted", *rule.SecurityGroupRuleId))
					} else {
						logger.Error(err, fmt.Sprintf("Unable to delete SG Egress Rule with id %s", *rule.SecurityGroupRuleId))
						return err
					}
				}
			} else {
				_, err := r.Ec2Client.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
					GroupId:              nlbSecurityGroupId,
					SecurityGroupRuleIds: []string{*rule.SecurityGroupRuleId},
				})
				if err != nil {
					var apiErr smithy.APIError
					errors.As(err, &apiErr)
					if "InvalidSecurityGroupRuleId.NotFound" == apiErr.ErrorCode() {
						logger.Info(fmt.Sprintf("SG Rule %s probably already deleted", *rule.SecurityGroupRuleId))
					} else {
						logger.Error(err, fmt.Sprintf("Unable to delete SG Ingress Rule with id %s", *rule.SecurityGroupRuleId))
						return err
					}
				}
			}
		}
	}
	return nil
}

func (r *NetworkLoadBalancerReconciler) handleFinalizer(ctx context.Context,
	networkLoadBalancer *awsv1.NetworkLoadBalancer) (*ctrl.Result, error) {
	if networkLoadBalancer.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(networkLoadBalancer, finalizerName) {
			controllerutil.AddFinalizer(networkLoadBalancer, finalizerName)
			if err := r.Update(ctx, networkLoadBalancer); err != nil {
				return &ctrl.Result{}, err
			}
		}
	} else {
		if controllerutil.ContainsFinalizer(networkLoadBalancer, finalizerName) {
			if err := r.deleteExternalResources(ctx, networkLoadBalancer); err != nil {
				return &ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(networkLoadBalancer, finalizerName)
			if err := r.Update(ctx, networkLoadBalancer); err != nil {
				return &ctrl.Result{}, err
			}
		}
		return &ctrl.Result{}, nil
	}
	return nil, nil
}

func (r *NetworkLoadBalancerReconciler) createListeners(ctx context.Context,
	nlbArn *string,
	networkLoadBalancer *awsv1.NetworkLoadBalancer,
	commonTags []Tag) error {
	logger := log.FromContext(ctx)
	healthCheckInterval := int32(10)
	healthyThreshold := int32(2)
	for _, listener := range networkLoadBalancer.Spec.Listeners {
		logger.Info(fmt.Sprintf("Initializing listener with port %d protocol %s target service %s target service port %d",
			listener.Port, listener.Protocol, listener.Service, listener.ServicePort))
		var targetService v1.Service
		namespacedName := types.NamespacedName{
			Name:      listener.Service,
			Namespace: networkLoadBalancer.Namespace,
		}
		err := r.Get(ctx, namespacedName, &targetService)
		if err != nil {
			logger.Error(err, fmt.Sprintf("Unable to fetch service %s", listener.Service))
		}
		if targetService.Spec.Type != v1.ServiceTypeNodePort {
			return errors.New(fmt.Sprintf("Service %s is not of type NodePort", listener.Service))
		}
		for _, servicePort := range targetService.Spec.Ports {
			listenerPort := int32(listener.Port)
			listenerServicePort := int32(listener.ServicePort)
			if listenerServicePort == servicePort.Port && v1.Protocol(listener.Protocol) == servicePort.Protocol {
				suffix := fmt.Sprintf("%s-%s-%s-%d",
					targetService.Namespace,
					targetService.Name,
					servicePort.Protocol,
					servicePort.Port)
				tgName := fmt.Sprintf("pomidor-tg-%s", util.Sha256String(suffix)[:20])
				logger.Info(fmt.Sprintf("Creating target group %s with port %d based on service %s protocol %s service port %d",
					tgName, servicePort.NodePort, targetService.Name, servicePort.Protocol, servicePort.Port))
				tgOut, err := r.ElbClient.CreateTargetGroup(ctx, &elbv2.CreateTargetGroupInput{
					Name:                       &tgName,
					Protocol:                   elb2types.ProtocolEnum(servicePort.Protocol),
					TargetType:                 elb2types.TargetTypeEnumInstance,
					VpcId:                      &r.VpcId,
					Tags:                       funk.Map(commonTags, func(tag Tag) elb2types.Tag { return tag.asElbTag() }).([]elb2types.Tag),
					Port:                       &servicePort.NodePort,
					HealthCheckIntervalSeconds: &healthCheckInterval,
					HealthyThresholdCount:      &healthyThreshold,
				})
				if err != nil {
					return err
				}
				targetGroupArn := tgOut.TargetGroups[0].TargetGroupArn
				logger.Info(fmt.Sprintf("Target group %s created", *targetGroupArn))
				for _, asg := range r.NodesAsgNames {
					logger.Info(fmt.Sprintf("Trying to attach target group %s to ASG %s", *targetGroupArn, asg))
					_, err := r.AsgClient.AttachTrafficSources(ctx, &autoscaling.AttachTrafficSourcesInput{
						AutoScalingGroupName: &asg,
						TrafficSources: []asgtypes.TrafficSourceIdentifier{
							{
								Identifier: targetGroupArn,
							},
						},
					})
					if err != nil {
						return err
					}
				}
				elbOut, err := r.ElbClient.CreateListener(ctx, &elbv2.CreateListenerInput{
					LoadBalancerArn: nlbArn,
					Tags:            funk.Map(commonTags, func(tag Tag) elb2types.Tag { return tag.asElbTag() }).([]elb2types.Tag),
					Port:            &listenerPort,
					Protocol:        elb2types.ProtocolEnum(listener.Protocol),
					DefaultActions: []elb2types.Action{
						{
							Type:           elb2types.ActionTypeEnumForward,
							TargetGroupArn: targetGroupArn,
						},
					},
				})
				if err != nil {
					return err
				}
				listenerArn := elbOut.Listeners[0].ListenerArn
				logger.Info(fmt.Sprintf("Listener %s created", *listenerArn))
				break
			} else {
				logger.Info(fmt.Sprintf("Listener with servciePort %d and protocol %s doesn't match service %s with port %d and protocol %s",
					listener.ServicePort, listener.Protocol, targetService.Name, servicePort.Port, servicePort.Protocol))
			}
		}
	}
	return nil
}

func (r *NetworkLoadBalancerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	namespacedName := req.NamespacedName
	var networkLoadBalancer awsv1.NetworkLoadBalancer
	err := r.Get(ctx, namespacedName, &networkLoadBalancer)
	if err != nil {
		notFoundIgnored := client.IgnoreNotFound(err)
		if notFoundIgnored != nil {
			logger.Error(err, "Unable to fetch NetworkLoadBalancer")
		}
		return ctrl.Result{}, notFoundIgnored
	}

	out, err := r.handleFinalizer(ctx, &networkLoadBalancer)
	if out != nil {
		return *out, err
	}

	suffix := fmt.Sprintf("%s-%s-%s", r.ClusterName, namespacedName.Namespace, namespacedName.Name)
	lbName := fmt.Sprintf("pomidor-nlb-%s", util.Sha256String(suffix)[:20])
	isInternal := networkLoadBalancer.Spec.Internal
	var lbScheme elb2types.LoadBalancerSchemeEnum
	var subnets []string
	if !isInternal {
		lbScheme = elb2types.LoadBalancerSchemeEnumInternetFacing
		subnets = r.PublicSubnets
	} else {
		lbScheme = elb2types.LoadBalancerSchemeEnumInternal
		subnets = r.PrivateSubnets
	}
	sgDescription := fmt.Sprintf("%s %s NLB Security Group", namespacedName.Namespace, namespacedName.Name)
	sgName := fmt.Sprintf("nlb-sg-%s", util.Sha256String(suffix)[:32])
	commonTags := []Tag{
		{
			Key:   aws2.String(clusterTag),
			Value: &r.ClusterName,
		},
		{
			Key:   aws2.String(managedByTag),
			Value: aws2.String(pomidor),
		},
		{
			Key:   aws2.String(namespaceTag),
			Value: &namespacedName.Namespace,
		},
		{
			Key:   aws2.String(resourceTag),
			Value: &namespacedName.Name,
		},
	}
	securityGroupId, err := aws.CreateSecurityGroup(r.Ec2Client,
		ctx,
		&sgName,
		&sgDescription,
		&r.VpcId,
		[]ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeSecurityGroup,
				Tags:         funk.Map(commonTags, func(tag Tag) ec2types.Tag { return tag.asEc2Tag() }).([]ec2types.Tag),
			},
		})
	if err != nil {
		logger.Error(err, "Unable to create SecurityGroup")
		r.Recorder.Event(&networkLoadBalancer, "Warning", "Failed", "Unable to create Security Group")
		return ctrl.Result{}, err
	}
	err = r.deleteSecurityGroupRules(ctx, securityGroupId)
	if err != nil {
		r.Recorder.Event(&networkLoadBalancer, "Warning", "Failed", "Unable to prepare security group")
		return ctrl.Result{}, err
	}
	sgIds := []string{*securityGroupId}
	err = r.createIngressRules(ctx, &networkLoadBalancer, securityGroupId)
	if err != nil {
		r.Recorder.Event(&networkLoadBalancer, "Warning", "Failed", "Unable to create ingress rules")
		return ctrl.Result{}, err
	}
	err = r.createEgressRule(ctx, securityGroupId)
	if err != nil {
		r.Recorder.Event(&networkLoadBalancer, "Warning", "Failed", "Unable to create egress rule")
		return ctrl.Result{}, err
	}
	ruleId, err := r.addClusterSecurityGroupIngressRule(ctx, securityGroupId)
	if err != nil {
		r.Recorder.Event(&networkLoadBalancer, "Warning", "Failed", "Unable to update Cluster SG")
		return ctrl.Result{}, err
	}
	lbOut, err := r.ElbClient.CreateLoadBalancer(ctx, &elbv2.CreateLoadBalancerInput{
		Name:           &lbName,
		Type:           elb2types.LoadBalancerTypeEnumNetwork,
		Scheme:         lbScheme,
		Subnets:        subnets,
		SecurityGroups: sgIds,
		Tags:           funk.Map(commonTags, func(tag Tag) elb2types.Tag { return tag.asElbTag() }).([]elb2types.Tag),
	})
	if err != nil {
		logger.Error(err, "Unable to create Network Load Balancer")
		r.Recorder.Event(&networkLoadBalancer, "Warning", "Failed", "Unable to create NLB")
		return ctrl.Result{}, err
	}
	nlbArn := lbOut.LoadBalancers[0].LoadBalancerArn
	err = r.createListeners(ctx, nlbArn, &networkLoadBalancer, commonTags)
	if err != nil {
		logger.Error(err, "Unable to create listeners")
		r.Recorder.Event(&networkLoadBalancer, "Warning", "Failed", "Unable to create NLB Listeners")
		return ctrl.Result{}, err
	}
	networkLoadBalancer.Status.Arn = nlbArn
	networkLoadBalancer.Status.DnsName = lbOut.LoadBalancers[0].DNSName
	networkLoadBalancer.Status.SecurityGroupIds = sgIds
	networkLoadBalancer.Status.ClusterSgRuleId = ruleId
	err = r.Status().Update(ctx, &networkLoadBalancer)
	if err != nil {
		logger.Error(err, "Unable to update status")
		return ctrl.Result{}, err
	}

	r.Recorder.Event(&networkLoadBalancer, "Normal", "Created", "NLB Created")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NetworkLoadBalancerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&awsv1.NetworkLoadBalancer{}).
		Complete(r)
}
