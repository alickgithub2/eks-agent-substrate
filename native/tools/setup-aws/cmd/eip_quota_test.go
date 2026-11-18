package cmd

import (
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestQuotaEIPsUsed(t *testing.T) {
	addrs := []ec2types.Address{
		{}, {}, {ServiceManaged: ec2types.ServiceManagedAlb}, {ServiceManaged: "rnat"}, {},
	}
	if got := quotaEIPsUsed(addrs); got != 3 {
		t.Errorf("quotaEIPsUsed = %d, want 3 (service-managed addresses are not counted)", got)
	}
}
