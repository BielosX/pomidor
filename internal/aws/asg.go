package aws

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

func GetAsgElbV2TrafficSources(asgClient *autoscaling.Client, ctx context.Context, asgName string) ([]types.TrafficSourceState, error) {
	var result []types.TrafficSourceState
	paginator := autoscaling.NewDescribeTrafficSourcesPaginator(asgClient, &autoscaling.DescribeTrafficSourcesInput{
		TrafficSourceType:    aws.String("elbv2"),
		AutoScalingGroupName: &asgName,
	})
	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, out.TrafficSources...)
	}
	return result, nil
}
