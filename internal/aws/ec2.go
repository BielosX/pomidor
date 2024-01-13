package aws

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var groupNameFilter = "group-name"
var groupRuleTagName = "Name"

func CreateSecurityGroup(ec2Client *ec2.Client,
	ctx context.Context,
	groupName *string,
	groupDescription *string,
	VpcId *string,
	tagSpec []types.TagSpecification) (*string, error) {
	logger := log.FromContext(ctx)
	var groupId *string
	out, err := ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:         groupName,
		Description:       groupDescription,
		VpcId:             VpcId,
		TagSpecifications: tagSpec,
	})
	if err != nil {
		var apiErr smithy.APIError
		errors.As(err, &apiErr)
		if "InvalidGroup.Duplicate" == apiErr.ErrorCode() {
			logger.Info(fmt.Sprintf("Security Group with name %s already exists", *groupName))
			out, err := ec2Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
				Filters: []types.Filter{
					{
						Name:   &groupNameFilter,
						Values: []string{*groupName},
					},
				},
			})
			if err != nil {
				return nil, err
			}
			groupId = out.SecurityGroups[0].GroupId
		} else {
			return nil, err
		}
	} else {
		groupId = out.GroupId
	}
	return groupId, nil
}

func AuthorizeSecurityGroup(ec2Client *ec2.Client,
	ctx context.Context,
	gropId *string,
	egress bool,
	ruleName string,
	ipPermissions []types.IpPermission) (*string, error) {
	logger := log.FromContext(ctx)
	var actionError error
	var ruleId *string
	tagSpec := types.TagSpecification{
		ResourceType: types.ResourceTypeSecurityGroupRule,
		Tags: []types.Tag{
			{
				Key:   &groupRuleTagName,
				Value: &ruleName,
			},
		},
	}
	if !egress {
		out, err := ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId:           gropId,
			IpPermissions:     ipPermissions,
			TagSpecifications: []types.TagSpecification{tagSpec},
		})
		if err == nil {
			ruleId = out.SecurityGroupRules[0].SecurityGroupRuleId
		}
		actionError = err
	} else {
		out, err := ec2Client.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
			GroupId:           gropId,
			IpPermissions:     ipPermissions,
			TagSpecifications: []types.TagSpecification{tagSpec},
		})
		if err == nil {
			ruleId = out.SecurityGroupRules[0].SecurityGroupRuleId
		}
		actionError = err
	}
	if actionError != nil {
		var apiErr smithy.APIError
		errors.As(actionError, &apiErr)
		if "InvalidPermission.Duplicate" == apiErr.ErrorCode() {
			logger.Info("Security Group Rule already exists")
			filter := "tag:Name"
			out, err := ec2Client.DescribeSecurityGroupRules(ctx, &ec2.DescribeSecurityGroupRulesInput{
				Filters: []types.Filter{
					{
						Name:   &filter,
						Values: []string{ruleName},
					},
				},
			})
			if err != nil {
				return nil, err
			}
			ruleId = out.SecurityGroupRules[0].SecurityGroupRuleId
		} else {
			return nil, actionError
		}
	}
	return ruleId, nil
}
