// Copyright 2023 The Atlas Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ariga.io/atlas/atlasexec"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/stretchr/testify/require"
)

func Test_mergeBlocks(t *testing.T) {
	type args struct {
		atlasEnv string
		dst      string
		src      string
	}
	tests := []struct {
		name     string
		args     args
		expected string
	}{
		{
			name: "empty",
			args: args{
				dst: "",
				src: "",
			},
			expected: "",
		},
		{
			name: "dst empty",
			args: args{
				dst: "",
				src: `env "example" {}`,
			},
			expected: `
env "example" {}`,
		},
		{
			name: "same block",
			args: args{
				dst: `env "example" {}`,
				src: `env "example" {}`,
			},
			expected: `env "example" {}`,
		},
		{
			name: "different block",
			args: args{
				dst: `env "example" {}`,
				src: `env "example2" {}`,
			},
			expected: `env "example" {}
env "example2" {}`,
		},
		{
			name: "same block with different attributes",
			args: args{
				dst: `
env "example" {
	key = "value"
}`,
				src: `
env "example" {
	key2 = "value2"
}`,
			},
			expected: `
env "example" {
  key  = "value"
  key2 = "value2"
}`,
		},
		{
			name: "same block with same attributes",
			args: args{
				dst: `
env "example" {
	key = "value"
}`,
				src: `
env "example" {
	key = "value2"
}`,
			},
			expected: `
env "example" {
  key = "value2"
}`,
		},
		{
			name: "merge unnamed blocks",
			args: args{
				dst: `
env {
    name = atlas.env
	key = "value"
}`,
				src: `
env {
	name = atlas.env
	key2 = "value2"
}
`,
			},
			expected: `
env {
  name = atlas.env
  key  = "value"
  key2 = "value2"
}`,
		},
		{
			name: "merge named env block to unnamed env block",
			args: args{
				dst: `
env {
    name = atlas.env
	key = "value"
}`,
				src: `
env "example" {
	key2 = "value2"
}
`,
			},
			expected: `
env {
  name = atlas.env
  key  = "value"
  key2 = "value2"
}`,
		},
		{
			name: "merge unnamed block to named block",
			args: args{
				atlasEnv: "example",
				dst: `
env "example" {
	key = "value"
}`,
				src: `
env {
	name = atlas.env
	key2 = "value2"
}
`,
			},
			expected: `
env "example" {
  key  = "value"
  key2 = "value2"
}`,
		},
		{
			name: "two diff blocks - unlabeled and labeled clickhouse",
			args: args{
				dst: `
env "kubernetes" {
  diff {
    skip {
      drop_schema = true
      drop_table  = true
    }
  }
  diff "clickhouse" {
    cluster {
      name = "{cluster}"
    }
  }
}`,
				src: ``,
			},
			expected: `
env "kubernetes" {
  diff {
    skip {
      drop_schema = true
      drop_table  = true
    }
  }
  diff "clickhouse" {
    cluster {
      name = "{cluster}"
    }
  }
}`,
		},
		{
			name: "merge user config diff clickhouse into operator diff",
			args: args{
				atlasEnv: "kubernetes",
				dst: `
env "kubernetes" {
  diff {
    skip {
      drop_column = true
    }
  }
}`,
				src: `
env "kubernetes" {
  diff {
    skip {
      drop_schema = true
      drop_table  = true
    }
  }
  diff "clickhouse" {
    cluster {
      name = "{cluster}"
    }
  }
}`,
			},
			expected: `
env "kubernetes" {
  diff {
    skip {
      drop_column = true
      drop_schema = true
      drop_table  = true
    }
  }
  diff "clickhouse" {
    cluster {
      name = "{cluster}"
    }
  }
}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst, _ := hclwrite.ParseConfig([]byte(tt.args.dst), "", hcl.InitialPos)
			src, _ := hclwrite.ParseConfig([]byte(tt.args.src), "", hcl.InitialPos)
			mergeBlocks(dst.Body(), src.Body(), tt.args.atlasEnv)
			if got := string(dst.Bytes()); got != tt.expected {
				t.Errorf("mergeBlocks() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func Test_backoffDelayAt(t *testing.T) {
	type args struct {
		retry int
	}
	tests := []struct {
		name string
		args args
		want time.Duration
	}{
		{
			name: "0",
			args: args{
				retry: 0,
			},
			want: 0,
		},
		{
			name: "1",
			args: args{
				retry: 1,
			},
			want: retryDuration,
		},
		{
			name: "2",
			args: args{
				retry: 2,
			},
			want: 2 * retryDuration,
		},
		{
			name: "20",
			args: args{
				retry: 20,
			},
			want: 100 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backoffDelayAt(tt.args.retry); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("backoffDelayAt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewAtlasExec_HomeAndCache(t *testing.T) {
	tests := []struct {
		name         string
		homeEnv      string
		dataDir      string
		resourceHome string
		wantHome     string
		wantCache    string
	}{
		{
			name:         "data dir overrides home root",
			homeEnv:      "/",
			dataDir:      "data",
			resourceHome: "ns/name",
			wantHome:     "data/ns/name",
			wantCache:    "data/ns/name/.cache",
		},
		{
			name:         "empty home falls back to tmp",
			homeEnv:      "",
			resourceHome: "ignored",
			wantHome:     "/tmp",
			wantCache:    "/tmp/.cache",
		},
		{
			name:         "root home falls back to tmp",
			homeEnv:      "/",
			resourceHome: "ignored",
			wantHome:     "/tmp",
			wantCache:    "/tmp/.cache",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			capture := filepath.Join(tmp, "env.txt")
			path := filepath.Join(tmp, "atlas")
			script := `#!/bin/sh
{
  printf 'HOME=%s\n' "${HOME}"
  printf 'XDG_CACHE_HOME=%s\n' "${XDG_CACHE_HOME}"
  printf 'GOCACHE=%s\n' "${GOCACHE}"
} > "${CAPTURE_ENV}"
printf '{"Org":"test"}'
`
			require.NoError(t, os.WriteFile(path, []byte(script), 0755))
			t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CAPTURE_ENV", capture)
			t.Setenv("HOME", tt.homeEnv)
			if tt.dataDir == "" {
				t.Setenv(envDataDir, "")
			} else {
				dataDir := filepath.Join(tmp, tt.dataDir)
				require.NoError(t, os.MkdirAll(dataDir, 0755))
				t.Setenv(envDataDir, dataDir)
				tt.wantHome = filepath.Join(tmp, tt.wantHome)
				tt.wantCache = filepath.Join(tmp, tt.wantCache)
			}
			exec, err := NewAtlasExec(tmp, nil, tt.resourceHome)
			require.NoError(t, err)
			_, err = exec.WhoAmI(context.Background(), &atlasexec.WhoAmIParams{})
			require.NoError(t, err)
			require.DirExists(t, tt.wantCache)
			if tt.dataDir != "" {
				require.DirExists(t, tt.wantHome)
			}
			data, err := os.ReadFile(capture)
			require.NoError(t, err)
			got := make(map[string]string)
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if line == "" {
					continue
				}
				k, v, ok := strings.Cut(line, "=")
				require.True(t, ok)
				got[k] = v
			}
			require.Equal(t, map[string]string{
				"HOME":           tt.wantHome,
				"XDG_CACHE_HOME": tt.wantCache,
				"GOCACHE":        filepath.Join(tt.wantCache, "go-build"),
			}, got)
		})
	}
}
