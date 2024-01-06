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

package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/thoas/go-funk"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	eks "github.com/aws/aws-sdk-go-v2/service/eks"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	awsv1 "pomidor/pomidor/api/v1"
	"pomidor/pomidor/internal/controller"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(awsv1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func describeSubnetsWithTagKey(ec2Client *ec2.Client,
	ctx context.Context,
	vpcId string,
	tagKey string,
	nextToken *string) (*ec2.DescribeSubnetsOutput, error) {
	vpcFilterName := "vpc-id"
	tagFilterName := "tag-key"
	return ec2Client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		NextToken: nextToken,
		Filters: []types.Filter{
			{
				Name:   &vpcFilterName,
				Values: []string{vpcId},
			},
			{
				Name:   &tagFilterName,
				Values: []string{tagKey},
			},
		},
	})
}

func getSubnetIdsByTagKey(ec2Client *ec2.Client, ctx context.Context, vpcId string, tagKey string) ([]string, error) {
	var nextToken *string
	firstPageFetched := false
	var result []string
	for nextToken != nil || !firstPageFetched {
		out, err := describeSubnetsWithTagKey(ec2Client, ctx, vpcId, tagKey, nextToken)
		if err != nil {
			return nil, err
		}
		nextToken = out.NextToken
		subnetIds := funk.Map(out.Subnets, func(subnet types.Subnet) string {
			return *subnet.SubnetId
		}).([]string)
		result = append(result, subnetIds...)
		firstPageFetched = true
	}
	return result, nil
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var clusterName string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&clusterName, "cluster-name", "", "AWS EKS Cluster Name")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                enableLeaderElection,
		LeaderElectionID:              "5b2da014.pomidor",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		setupLog.Error(err, "Unable to load AWS Config")
		os.Exit(1)
	}

	elbClient := elbv2.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	eksClient := eks.NewFromConfig(cfg)
	eksOut, err := eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{
		Name: &clusterName,
	})
	if err != nil {
		setupLog.Error(err, "Unable to describe cluster")
		os.Exit(1)
	}
	vpcId := *eksOut.Cluster.ResourcesVpcConfig.VpcId
	privateSubnets, err := getSubnetIdsByTagKey(ec2Client, ctx, vpcId, "kubernetes.io/role/internal-elb")
	if err != nil {
		setupLog.Error(err, "Unable to fetch private subnets")
		os.Exit(1)
	}
	publicSubnets, err := getSubnetIdsByTagKey(ec2Client, ctx, vpcId, "kubernetes.io/role/elb")
	if err != nil {
		setupLog.Error(err, "Unable to fetch public subnets")
		os.Exit(1)
	}

	setupLog.Info(fmt.Sprintf("Running in cluster %s, VpcId: %s, public subnets: %v, private subnets: %v",
		clusterName,
		vpcId,
		publicSubnets,
		privateSubnets))

	if err = (&controller.NetworkLoadBalancerReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		Recorder:       mgr.GetEventRecorderFor("network-load-balancer-controller"),
		ElbClient:      elbClient,
		Ec2Client:      ec2Client,
		PrivateSubnets: privateSubnets,
		PublicSubnets:  publicSubnets,
		VpcId:          vpcId,
		ClusterName:    clusterName,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NetworkLoadBalancer")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
