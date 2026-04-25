package speaker

import (
	"net"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func TestValidateRequiredFlags(t *testing.T) {
	tests := []struct {
		name        string
		config      *Configuration
		expectError bool
		errContains []string
	}{
		{
			name: "all required flags provided with IPv4 neighbor",
			config: &Configuration{
				NeighborAddresses: []net.IP{net.ParseIP("192.168.1.1")},
				ClusterAs:         65000,
				NeighborAs:        65001,
				NodeName:          "node1",
			},
			expectError: false,
		},
		{
			name: "all required flags provided with IPv6 neighbor",
			config: &Configuration{
				NeighborIPv6Addresses: []net.IP{net.ParseIP("2001:db8::1")},
				ClusterAs:             65000,
				NeighborAs:            65001,
				NodeName:              "node1",
			},
			expectError: false,
		},
		{
			name: "all required flags provided with both IPv4 and IPv6 neighbors",
			config: &Configuration{
				NeighborAddresses:     []net.IP{net.ParseIP("192.168.1.1")},
				NeighborIPv6Addresses: []net.IP{net.ParseIP("2001:db8::1")},
				ClusterAs:             65000,
				NeighborAs:            65001,
				NodeName:              "node1",
			},
			expectError: false,
		},
		{
			name: "missing neighbor addresses",
			config: &Configuration{
				ClusterAs:  65000,
				NeighborAs: 65001,
				NodeName:   "node1",
			},
			expectError: true,
			errContains: []string{"neighbor-address", "neighbor-ipv6-address"},
		},
		{
			name: "missing cluster-as",
			config: &Configuration{
				NeighborAddresses: []net.IP{net.ParseIP("192.168.1.1")},
				NeighborAs:        65001,
				NodeName:          "node1",
			},
			expectError: true,
			errContains: []string{"cluster-as"},
		},
		{
			name: "missing neighbor-as",
			config: &Configuration{
				NeighborAddresses: []net.IP{net.ParseIP("192.168.1.1")},
				ClusterAs:         65000,
				NodeName:          "node1",
			},
			expectError: true,
			errContains: []string{"neighbor-as"},
		},
		{
			name: "missing node-name",
			config: &Configuration{
				NeighborAddresses: []net.IP{net.ParseIP("192.168.1.1")},
				ClusterAs:         65000,
				NeighborAs:        65001,
			},
			expectError: true,
			errContains: []string{"node-name"},
		},
		{
			name: "nat-gw mode does not require node-name",
			config: &Configuration{
				NeighborAddresses: []net.IP{net.ParseIP("192.168.1.1")},
				ClusterAs:         65000,
				NeighborAs:        65001,
				NatGwMode:         true,
			},
			expectError: false,
		},
		{
			name:        "missing all required flags",
			config:      &Configuration{},
			expectError: true,
			errContains: []string{"neighbor-address", "cluster-as", "neighbor-as", "node-name"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.validateRequiredFlags()
			if tt.expectError {
				require.Error(t, err)
				for _, s := range tt.errContains {
					require.Contains(t, err.Error(), s)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestLocalAddressFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want net.IP
	}{
		{
			name: "unset defaults to nil",
			args: []string{},
			want: nil,
		},
		{
			name: "IPv4 source address",
			args: []string{"--local-address=10.0.0.1"},
			want: net.ParseIP("10.0.0.1"),
		},
		{
			name: "IPv6 ULA source address",
			args: []string{"--local-address=fd0f:6160:5113::1"},
			want: net.ParseIP("fd0f:6160:5113::1"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
			got := fs.IP("local-address", nil, "")
			require.NoError(t, fs.Parse(tt.args))
			if tt.want == nil {
				require.Nil(t, *got)
			} else {
				require.True(t, tt.want.Equal(*got), "expected %s, got %s", tt.want, *got)
			}
		})
	}
}
