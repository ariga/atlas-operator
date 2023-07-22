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
				User:     "user",
				Password: "pass",
				Host:     "host",
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
				Scheme: "sqlite",
				Host:   "file",
				Parameters: map[string]string{
					"mode": "memory",
				},
			},
			exp: "sqlite://file?mode=memory",
		},
		{
			c: Credentials{
				Scheme:   "mysql",
				User:     "user",
				Password: "pass",
				Host:     "host",
				Database: "db",
			},
			exp: "mysql://user:pass@host/db",
		},
		{
			c: Credentials{
				Scheme:   "mysql",
				User:     "user",
				Password: "pass",
				Host:     "",
				Port:     3306,
				Database: "db",
			},
			exp: "mysql://user:pass@:3306/db",
		},
	} {
		t.Run(tt.exp, func(t *testing.T) {
			require.Equal(t, tt.exp, tt.c.URL().String())
		})
	}
}
