package devenv

import "testing"

func TestHasRolePrefix(t *testing.T) {
	const identityCenterPrefix = "arn:aws:iam::123456789012:role/aws-reserved/sso.amazonaws.com/us-west-2/AWSReservedSSO_Developer_"
	tests := []struct {
		name    string
		roleARN string
		prefix  string
		want    bool
	}{
		{
			name:    "pathless STS role",
			roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_Developer_deadbeef",
			prefix:  identityCenterPrefix,
			want:    true,
		},
		{
			name:    "pathful IAM role",
			roleARN: "arn:aws:iam::123456789012:role/aws-reserved/sso.amazonaws.com/us-west-2/AWSReservedSSO_Developer_deadbeef",
			prefix:  identityCenterPrefix,
			want:    true,
		},
		{
			name:    "wrong account",
			roleARN: "arn:aws:iam::210987654321:role/AWSReservedSSO_Developer_deadbeef",
			prefix:  identityCenterPrefix,
			want:    false,
		},
		{
			name:    "near prefix",
			roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_DeveloperOps_deadbeef",
			prefix:  identityCenterPrefix,
			want:    false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasRolePrefix(test.roleARN, []string{test.prefix}); got != test.want {
				t.Fatalf("hasRolePrefix() = %t, want %t", got, test.want)
			}
		})
	}
}
