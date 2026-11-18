package cmd

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestPrivateSubnets(t *testing.T) {
	sub := func(id, az string, public bool) ec2types.Subnet {
		return ec2types.Subnet{SubnetId: aws.String(id), AvailabilityZone: aws.String(az), MapPublicIpOnLaunch: aws.Bool(public)}
	}
	subnets := []ec2types.Subnet{
		sub("subnet-a1", "us-east-1a", false), sub("subnet-a0", "us-east-1a", false),
		sub("subnet-b1", "us-east-1b", false), sub("subnet-c1", "us-east-1c", true),
		sub("subnet-d1", "us-east-1d", false), sub("subnet-e1", "us-east-1e", false),
	}
	nat := ec2types.RouteTable{Routes: []ec2types.Route{{DestinationCidrBlock: aws.String("0.0.0.0/0"), NatGatewayId: aws.String("nat-1")}},
		Associations: []ec2types.RouteTableAssociation{{SubnetId: aws.String("subnet-a1")}, {SubnetId: aws.String("subnet-a0")}, {SubnetId: aws.String("subnet-c1")}}}
	igw := ec2types.RouteTable{Routes: []ec2types.Route{{DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String("igw-1")}},
		Associations: []ec2types.RouteTableAssociation{{SubnetId: aws.String("subnet-d1")}}}
	main := ec2types.RouteTable{Routes: []ec2types.Route{{DestinationCidrBlock: aws.String("0.0.0.0/0"), NatGatewayId: aws.String("nat-2")}},
		Associations: []ec2types.RouteTableAssociation{{Main: aws.Bool(true)}}}
	got := privateSubnets(subnets, []ec2types.RouteTable{nat, igw, main}, []string{"us-east-1a", "us-east-1b", "us-east-1c", "us-east-1d"})
	var ids []string
	for _, s := range got {
		ids = append(ids, aws.ToString(s.SubnetId))
	}
	// a0 (first in 1a), b1 (main table via NAT); not c1 (public IPs), d1
	// (internet gateway), e1 (zone not usable).
	if len(ids) != 2 || ids[0] != "subnet-a0" || ids[1] != "subnet-b1" {
		t.Fatalf("privateSubnets = %v, want [subnet-a0 subnet-b1]", ids)
	}
}
