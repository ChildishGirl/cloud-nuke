package resources

import (
	"context"
	"fmt"
	"time"

	awsgo "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/gruntwork-io/cloud-nuke/config"
	"github.com/gruntwork-io/cloud-nuke/logging"
	r "github.com/gruntwork-io/cloud-nuke/report" // Alias the package as 'r'
	"github.com/gruntwork-io/cloud-nuke/util"
	"github.com/gruntwork-io/go-commons/errors"
)

// shouldIncludeSecurityGroup determines whether a security group should be included for deletion based on the provided configuration.
func shouldIncludeSecurityGroup(sg types.SecurityGroup, firstSeenTime *time.Time, configObj config.Config) bool {
	var groupName = sg.GroupName

	if !configObj.SecurityGroup.DefaultOnly && *groupName == "default" {
		logging.Debugf("[default security group] skipping default security group including")
		return false
	}

	return configObj.SecurityGroup.ShouldInclude(config.ResourceValue{
		Name: groupName,
		Tags: util.ConvertTypesTagsToMap(sg.Tags),
		Time: firstSeenTime,
	})
}

// getAll retrieves all security group identifiers based on the provided configuration.
func (sg *SecurityGroup) getAll(c context.Context, configObj config.Config) ([]*string, error) {
	var identifiers []*string
	var firstSeenTime *time.Time
	var err error

	var filters []types.Filter
	if configObj.SecurityGroup.DefaultOnly {
		// Note : we can't simply remove the default security groups. Instead, we're only able to revoke permissions on the security group rules.
		// Setting a flag that can be accessed within the nuke method to check if the nuking is for default or not.
		sg.NukeOnlyDefault = configObj.SecurityGroup.DefaultOnly

		logging.Debugf("[default only] Retrieving the default security-groups")
		filters = []types.Filter{
			{
				Name:   awsgo.String("group-name"),
				Values: []string{"default"},
			},
		}
	}

	resp, err := sg.Client.DescribeSecurityGroups(sg.Context, &ec2.DescribeSecurityGroupsInput{
		Filters: filters,
	})
	if err != nil {
		logging.Debugf("[Security Group] Failed to list security groups: %s", err)
		return nil, errors.WithStackTrace(err)
	}

	for _, group := range resp.SecurityGroups {
		firstSeenTime, err = util.GetOrCreateFirstSeen(c, sg.Client, group.GroupId, util.ConvertTypesTagsToMap(group.Tags))
		if err != nil {
			logging.Error("unable to retrieve first seen tag")
			return nil, errors.WithStackTrace(err)
		}

		if shouldIncludeSecurityGroup(group, firstSeenTime, configObj) {
			identifiers = append(identifiers, group.GroupId)
		}
	}

	// Check and verify the list of allowed nuke actions
	sg.VerifyNukablePermissions(identifiers, func(id *string) error {
		_, err := sg.Client.DeleteSecurityGroup(sg.Context, &ec2.DeleteSecurityGroupInput{
			GroupId: id,
			DryRun:  awsgo.Bool(true),
		})
		return err
	})

	return identifiers, nil
}

func (sg *SecurityGroup) detachAssociatedSecurityGroups(id *string) error {
	logging.Debugf("[Security Group detach from dependancy] detaching the security group %s from dependant", awsgo.ToString(id))

	resp, err := sg.Client.DescribeSecurityGroups(sg.Context, &ec2.DescribeSecurityGroupsInput{})
	if err != nil {
		logging.Debugf("[Security Group] Failed to list security groups: %s", err)
		return errors.WithStackTrace(err)
	}

	for _, securityGroup := range resp.SecurityGroups {
		// omit the check for current security group
		if awsgo.ToString(id) == awsgo.ToString(securityGroup.GroupId) {
			continue
		}

		hasMatching, revokeIpPermissions := hasMatchingGroupIdRule(id, securityGroup.IpPermissions)
		if hasMatching && len(revokeIpPermissions) > 0 {
			logging.Debugf("[Security Group revoke ingress] revoking the ingress rules of %s", awsgo.ToString(securityGroup.GroupId))
			_, err := sg.Client.RevokeSecurityGroupIngress(sg.Context, &ec2.RevokeSecurityGroupIngressInput{
				GroupId:       securityGroup.GroupId,
				IpPermissions: revokeIpPermissions,
			})

			if err != nil {
				logging.Debugf("[Security Group] Failed to revoke ingress rules: %s", err)
				return errors.WithStackTrace(err)
			}
		}

		// check egress rule
		hasMatchingEgress, revokeIpPermissions := hasMatchingGroupIdRule(id, securityGroup.IpPermissionsEgress)
		if hasMatchingEgress && len(revokeIpPermissions) > 0 {
			logging.Debugf("[Security Group revoke ingress] revoking the egress rules of %s", awsgo.ToString(securityGroup.GroupId))
			_, err := sg.Client.RevokeSecurityGroupEgress(sg.Context, &ec2.RevokeSecurityGroupEgressInput{
				GroupId:       securityGroup.GroupId,
				IpPermissions: revokeIpPermissions,
			})
			if err != nil {
				logging.Debugf("[Security Group] Failed to revoke egress rules: %s", err)
				return errors.WithStackTrace(err)
			}
		}

	}
	return nil
}

func hasMatchingGroupIdRule(checkingGroup *string, IpPermission []types.IpPermission) (bool, []types.IpPermission) {
	var hasMatching bool
	var revokeIpPermissions []types.IpPermission

	for _, ipPermission := range IpPermission {
		revokeIdGroupPairs := make([]types.UserIdGroupPair, 0) // Create a new slice to store filtered pairs

		for _, pair := range ipPermission.UserIdGroupPairs {
			// Check if GroupId match the checkingGroup
			if awsgo.ToString(pair.GroupId) == awsgo.ToString(checkingGroup) {
				revokeIdGroupPairs = append(revokeIdGroupPairs, pair) // Append to the filtered slice
				hasMatching = true                                    // Set the flag if a match is found
			}
		}

		if len(revokeIdGroupPairs) > 0 {
			ipPermission.UserIdGroupPairs = revokeIdGroupPairs
			revokeIpPermissions = append(revokeIpPermissions, ipPermission)
		}
	}

	return hasMatching, revokeIpPermissions
}

func (sg *SecurityGroup) nuke(id *string) error {

	if err := sg.terminateInstancesAssociatedWithSecurityGroup(*id); err != nil {
		return errors.WithStackTrace(err)
	}

	if err := sg.detachAssociatedSecurityGroups(id); err != nil {
		return errors.WithStackTrace(err)
	}

	// check the nuking is only for default security groups, then nuke and return
	if sg.NukeOnlyDefault {
		// RevokeSecurityGroupIngress
		if err := revokeSecurityGroupIngress(sg.Client, id); err != nil {
			return errors.WithStackTrace(err)
		}

		// RevokeSecurityGroupEgress
		if err := revokeSecurityGroupEgress(sg.Client, id); err != nil {
			return errors.WithStackTrace(err)
		}

		// RevokeIPv6SecurityGroupEgress
		if err := sg.RevokeIPv6SecurityGroupEgress(*id); err != nil {
			return errors.WithStackTrace(err)
		}

		return nil
	}

	// nuke the securiy group which is not default one
	if err := nukeSecurityGroup(sg.Client, id); err != nil {
		return errors.WithStackTrace(err)
	}
	return nil
}

func revokeSecurityGroupIngress(client SecurityGroupAPI, id *string) error {
	logging.Debug(fmt.Sprintf("Start revoking security groups ingress rule : %s", awsgo.ToString(id)))
	_, err := client.RevokeSecurityGroupIngress(context.TODO(), &ec2.RevokeSecurityGroupIngressInput{
		GroupId: id,
		IpPermissions: []types.IpPermission{
			{
				IpProtocol:       awsgo.String("-1"),
				FromPort:         awsgo.Int32(0),
				ToPort:           awsgo.Int32(0),
				UserIdGroupPairs: []types.UserIdGroupPair{{GroupId: id}},
			},
		},
	})
	if err != nil {
		if util.TransformAWSError(err) == util.ErrInvalidPermisionNotFound {
			logging.Debugf("Ingress rule not present (ok)")
			return nil
		}

		logging.Debugf("[Security Group] Failed to revoke security group ingress associated with security group %s: %s", awsgo.ToString(id), err)
		return errors.WithStackTrace(err)
	}
	logging.Debugf("Successfully revoked security group ingress rule: %s", awsgo.ToString(id))
	return nil
}

func revokeSecurityGroupEgress(client SecurityGroupAPI, id *string) error {
	logging.Debugf("Start revoking security groups ingress rule : %s", awsgo.ToString(id))

	_, err := client.RevokeSecurityGroupEgress(context.TODO(), &ec2.RevokeSecurityGroupEgressInput{
		GroupId: (id),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: awsgo.String("-1"),
				FromPort:   awsgo.Int32(0),
				ToPort:     awsgo.Int32(0),
				IpRanges:   []types.IpRange{{CidrIp: awsgo.String("0.0.0.0/0")}},
			},
		},
	})
	if err != nil {
		if util.TransformAWSError(err) == util.ErrInvalidPermisionNotFound {
			logging.Debugf("Egress rule not present (ok)")
			return nil
		}

		logging.Debugf("[Security Group] Failed to revoke security group egress associated with security group %s: %s", awsgo.ToString(id), err)
		return errors.WithStackTrace(err)
	}

	logging.Debugf("Successfully revoked security group egress rule: %s", awsgo.ToString(id))

	return nil
}

func (sg *SecurityGroup) RevokeIPv6SecurityGroupEgress(id string) error {
	_, err := sg.Client.RevokeSecurityGroupEgress(sg.Context, &ec2.RevokeSecurityGroupEgressInput{
		GroupId: awsgo.String(id),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: awsgo.String("-1"),
				FromPort:   awsgo.Int32(0),
				ToPort:     awsgo.Int32(0),
				Ipv6Ranges: []types.Ipv6Range{{CidrIpv6: awsgo.String("::/0")}},
			},
		},
	})
	if err != nil {
		if util.TransformAWSError(err) == util.ErrInvalidPermisionNotFound {
			logging.Debugf("Ingress rule not present (ok)")
			return nil
		}

		logging.Debugf("[Security Group] Failed to revoke security group egress associated with security group %s: %s", id, err)
		return errors.WithStackTrace(err)
	}

	return nil
}

func (sg *SecurityGroup) terminateInstancesAssociatedWithSecurityGroup(id string) error {

	resp, err := sg.Client.DescribeInstances(sg.Context, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   awsgo.String("instance.group-id"),
				Values: []string{id},
			},
		},
	})
	if err != nil {
		logging.Debugf("[Security Group] Failed to describe instances associated with security group %s: %s", id, err)
		return errors.WithStackTrace(err)
	}

	for _, reservation := range resp.Reservations {
		for _, instance := range reservation.Instances {
			instanceID := awsgo.ToString(instance.InstanceId)

			// Needs to release the elastic ips attached on the instance before nuking
			if err := sg.releaseEIPs([]*string{instance.InstanceId}); err != nil {
				logging.Debugf("[Failed EIP release] %s", err)
				return errors.WithStackTrace(err)
			}

			// terminating the instances which used this security group
			if _, err := sg.Client.TerminateInstances(sg.Context, &ec2.TerminateInstancesInput{
				InstanceIds: []string{instanceID},
			}); err != nil {
				logging.Debugf("[Failed] Ec2 termination %s", err)
				return errors.WithStackTrace(err)
			}

			logging.Debugf("[Instance Termination] waiting to terminate instance %s", instanceID)
			ec2Client, ok := sg.Client.(*ec2.Client)
			if !ok {
				return errors.WithStackTrace(err)
			}
			// wait until the instance terminated.
			err = waitUntilInstanceTerminated(ec2Client, sg.Context, instanceID)
			if err != nil {
				logging.Debugf("[Security Group] Failed to terminate instance %s associated with security group %s: %s", instanceID, id, err)
				return errors.WithStackTrace(err)
			}

			logging.Debugf("Terminated instance %s associated with security group %s", instanceID, id)
		}
	}

	return nil
}

func (sg *SecurityGroup) releaseEIPs(instanceIds []*string) error {
	logging.Debugf("Releasing Elastic IP address(s) associated on instances")
	for _, instanceID := range instanceIds {

		// get the elastic ip's associated with the EC2's
		output, err := sg.Client.DescribeAddresses(sg.Context, &ec2.DescribeAddressesInput{
			Filters: []types.Filter{
				{
					Name: awsgo.String("instance-id"),
					Values: []string{
						*instanceID,
					},
				},
			},
		})
		if err != nil {
			return err
		}

		for _, address := range output.Addresses {
			if _, err := sg.Client.ReleaseAddress(sg.Context, &ec2.ReleaseAddressInput{
				AllocationId: address.AllocationId,
			}); err != nil {
				logging.Debugf("An error happened while releasing the elastic ip address %s, error %v", *address.AllocationId, err)
				continue
			}

			logging.Debugf("Released Elastic IP address %s from instance %s", *address.AllocationId, *instanceID)
		}
	}

	return nil
}

func nukeSecurityGroup(client SecurityGroupAPI, id *string) error {
	logging.Debugf("Deleting security group %s", awsgo.ToString(id))

	if _, err := client.DeleteSecurityGroup(context.TODO(), &ec2.DeleteSecurityGroupInput{
		GroupId: id,
	}); err != nil {
		logging.Debugf("[Security Group] Failed to delete security group %s: %s", awsgo.ToString(id), err)
		return errors.WithStackTrace(err)
	}
	logging.Debugf("Deleted security group %s", awsgo.ToString(id))
	return nil
}

func (sg *SecurityGroup) nukeAll(identifiers []*string) error {
	if len(identifiers) == 0 {
		logging.Debugf("No security group identifiers to nuke in region %s", sg.Region)
		return nil
	}

	logging.Debugf("Deleting all security groups in region %s", sg.Region)
	var deletedGroups []*string

	for _, id := range identifiers {

		if nukable, reason := sg.IsNukable(*id); !nukable {
			logging.Debugf("[Skipping] %s nuke because %v", *id, reason)
			continue
		}

		err := sg.nuke(id)
		// Record status of this resource
		e := r.Entry{
			Identifier:   awsgo.ToString(id),
			ResourceType: "Security Group",
			Error:        err,
		}
		r.Record(e)

		if err != nil {
			logging.Debugf("[Failed] Error deleting security group %s: %s", *id, err)
		} else {
			deletedGroups = append(deletedGroups, id)
			logging.Debugf("Deleted security group: %s", *id)
		}
	}

	logging.Debugf("[OK] %d security group(s) deleted in %s", len(deletedGroups), sg.Region)

	return nil
}

// waitUntilInstanceTerminated checks the status of the instance until it is terminated.
func waitUntilInstanceTerminated(client *ec2.Client, ctx context.Context, instanceID string) error {
	waiter := ec2.NewInstanceTerminatedWaiter(client)

	// Configure the maximum wait time
	maxWaitTime := 5 * time.Minute

	// Call the waiter
	err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, maxWaitTime)
	if err != nil {
		return err
	}

	return nil
}
