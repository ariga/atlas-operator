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
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"k8s.io/apimachinery/pkg/types"
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

func Test_pendingTracker(t *testing.T) {
	var (
		tr   pendingTracker
		name = types.NamespacedName{Namespace: "default", Name: "schema"}
		now  = time.Now()
	)
	// The delay grows with the time spent waiting on the same key, up to the cap.
	for _, tt := range []struct {
		elapsed time.Duration
		want    time.Duration
	}{
		{0, 5 * time.Second},
		{5 * time.Second, 5 * time.Second},
		{6 * time.Second, 10 * time.Second},
		{10 * time.Second, 10 * time.Second},
		{15 * time.Second, 20 * time.Second},
		{30 * time.Second, 40 * time.Second},
		{time.Minute, 80 * time.Second},
		{100 * time.Second, 160 * time.Second},
		{3 * time.Minute, maxPendingDuration},
		{time.Hour, maxPendingDuration},
	} {
		if got := tr.delay(name, "plan1", now.Add(tt.elapsed)); got != tt.want {
			t.Errorf("delay(%v) = %v, want %v", tt.elapsed, got, tt.want)
		}
	}
	// A new plan starts from retryDuration, regardless of the previous one.
	now = now.Add(time.Hour)
	if got := tr.delay(name, "plan2", now); got != retryDuration {
		t.Errorf("delay(new key) = %v, want %v", got, retryDuration)
	}
	// Other resources are tracked separately.
	other := types.NamespacedName{Namespace: "default", Name: "other"}
	if got := tr.delay(other, "plan2", now.Add(time.Hour)); got != retryDuration {
		t.Errorf("delay(other resource) = %v, want %v", got, retryDuration)
	}
	// A reset resource starts from retryDuration.
	tr.reset(name)
	if got := tr.delay(name, "plan2", now.Add(time.Hour)); got != retryDuration {
		t.Errorf("delay(after reset) = %v, want %v", got, retryDuration)
	}
	// Simulate a resource that is polled until the delay reaches the cap.
	var (
		elapsed time.Duration
		delays  []time.Duration
	)
	tr.reset(name)
	for d := time.Duration(0); d < maxPendingDuration; elapsed += d {
		d = tr.delay(name, "plan3", now.Add(elapsed))
		delays = append(delays, d)
	}
	want := []time.Duration{
		5 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second,
		40 * time.Second, 80 * time.Second, 160 * time.Second, maxPendingDuration,
	}
	if !reflect.DeepEqual(delays, want) {
		t.Errorf("delays = %v, want %v", delays, want)
	}
}
