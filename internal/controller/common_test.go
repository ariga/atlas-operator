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
)

func Test_mergeBlocks(t *testing.T) {
	type args struct {
		dst string
		src string
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst, _ := hclwrite.ParseConfig([]byte(tt.args.dst), "", hcl.InitialPos)
			src, _ := hclwrite.ParseConfig([]byte(tt.args.src), "", hcl.InitialPos)
			mergeBlocks(dst.Body(), src.Body())
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
			want: 5 * time.Second,
		},
		{
			name: "2",
			args: args{
				retry: 2,
			},
			want: 10 * time.Second,
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
