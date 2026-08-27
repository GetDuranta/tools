package devenv

import "testing"

func TestHasRolePrefix(t *testing.T) {
	const developer = "arn:aws:iam::123456789012:role/aws-reserved/sso.amazonaws.com/us-west-2/AWSReservedSSO_Developer_"
	const developerTeam = "arn:aws:iam::123456789012:role/aws-reserved/sso.amazonaws.com/us-west-2/AWSReservedSSO_Developer_Team_"
	tests := []struct {
		name     string
		roleARN  string
		prefixes []string
		want     bool
	}{
		{
			name: "pathless STS role", roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_Developer_0123456789abcdef",
			prefixes: []string{developer}, want: true,
		},
		{
			name:     "pathful IAM role",
			roleARN:  "arn:aws:iam::123456789012:role/aws-reserved/sso.amazonaws.com/us-west-2/AWSReservedSSO_Developer_ABCDEF0123456789",
			prefixes: []string{developer}, want: true,
		},
		{
			name: "extended permission set", roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_Developer_Team_0123456789abcdef",
			prefixes: []string{developer}, want: false,
		},
		{
			name: "multiple prefixes", roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_Developer_Team_0123456789abcdef",
			prefixes: []string{developer, developerTeam}, want: true,
		},
		{
			name: "empty suffix", roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_Developer_",
			prefixes: []string{developer}, want: false,
		},
		{
			name: "short suffix", roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_Developer_0123456789abcde",
			prefixes: []string{developer}, want: false,
		},
		{
			name: "long suffix", roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_Developer_0123456789abcdef0",
			prefixes: []string{developer}, want: false,
		},
		{
			name: "non hex suffix", roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_Developer_0123456789abcdeg",
			prefixes: []string{developer}, want: false,
		},
		{
			name: "wrong account", roleARN: "arn:aws:iam::210987654321:role/AWSReservedSSO_Developer_0123456789abcdef",
			prefixes: []string{developer}, want: false,
		},
		{
			name: "wrong partition", roleARN: "arn:aws-us-gov:iam::123456789012:role/AWSReservedSSO_Developer_0123456789abcdef",
			prefixes: []string{developer}, want: false,
		},
		{
			name: "case mismatch", roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_developer_0123456789abcdef",
			prefixes: []string{developer}, want: false,
		},
		{
			name: "malformed path", roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_Developer_0123456789abcdef",
			prefixes: []string{"arn:aws:iam::123456789012:role/other/AWSReservedSSO_Developer_"}, want: false,
		},
		{
			name: "malformed service", roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_Developer_0123456789abcdef",
			prefixes: []string{"arn:aws:sts::123456789012:role/aws-reserved/sso.amazonaws.com/us-west-2/AWSReservedSSO_Developer_"}, want: false,
		},
		{
			name: "malformed account", roleARN: "arn:aws:iam::123456789012:role/AWSReservedSSO_Developer_0123456789abcdef",
			prefixes: []string{"arn:aws:iam::12345678901:role/aws-reserved/sso.amazonaws.com/us-west-2/AWSReservedSSO_Developer_"}, want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasRolePrefix(test.roleARN, test.prefixes); got != test.want {
				t.Fatalf("hasRolePrefix() = %t, want %t", got, test.want)
			}
		})
	}
}
