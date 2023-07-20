package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCredentials_URL(t *testing.T) {
	for _, tt := range []struct {
		c   Credentials
		exp string
	}{
		{
			c: Credentials{
				Scheme:   "postgres",
				Username: "user",
				Password: "pass",
				Hostname: "host",
				Port:     5432,
				Database: "db",
				Parameters: map[string]string{
					"sslmode": "disable",
				},
			},
			exp: "postgres://user:pass@host:5432/db?sslmode=disable",
		},
		{
			c: Credentials{
				Scheme:   "sqlite",
				Hostname: "file",
				Parameters: map[string]string{
					"mode": "memory",
				},
			},
			exp: "sqlite://file?mode=memory",
		},
	} {
		t.Run(tt.exp, func(t *testing.T) {
			require.Equal(t, tt.exp, tt.c.URL().String())
		})
	}
}
