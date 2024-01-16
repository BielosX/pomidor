package aws

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

func GetAsgElbV2TrafficSources(asgClient *autoscaling.Client, ctx context.Context, asgName string) ([]types.TrafficSourceState, error) {
	firstPageFetched := false
	var nextToken *string
	var result []types.TrafficSourceState
	for nextToken != nil || !firstPageFetched {
		firstPageFetched = true
		out, err := asgClient.DescribeTrafficSources(ctx, &autoscaling.DescribeTrafficSourcesInput{
			TrafficSourceType:    aws.String("elbv2"),
			AutoScalingGroupName: &asgName,
			NextToken:            nextToken,
		})
		if err != nil {
			return nil, err
		}
		nextToken = out.NextToken
		result = append(result, out.TrafficSources...)
	}
	return result, nil
}
