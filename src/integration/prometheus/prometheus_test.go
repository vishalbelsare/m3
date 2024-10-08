//go:build cluster_integration
// +build cluster_integration

//
// Copyright (c) 2021  Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package prometheus

import (
	"context"
	"path"
	"runtime"
	"testing"

	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/m3db/m3/src/integration/resources"
	"github.com/m3db/m3/src/integration/resources/docker/dockerexternal"
	"github.com/m3db/m3/src/integration/resources/inprocess"
)

func TestPrometheus(t *testing.T) {
	m3, prom, closer := testSetup(t)
	defer closer()

	RunTest(t, m3, prom)
}

func testSetup(t *testing.T) (resources.M3Resources, resources.ExternalResources, func()) {
	cfgs, err := inprocess.NewClusterConfigsFromYAML(
		TestPrometheusDBNodeConfig, TestPrometheusCoordinatorConfig, "",
	)
	require.NoError(t, err)

	m3, err := inprocess.NewCluster(cfgs,
		resources.ClusterOptions{
			DBNode: resources.NewDBNodeClusterOptions(),
		},
	)
	require.NoError(t, err)

	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	_, filename, _, _ := runtime.Caller(0)
	prom := dockerexternal.NewPrometheus(dockerexternal.PrometheusOptions{
		Pool:      pool,
		PathToCfg: path.Join(path.Dir(filename), "../resources/docker/dockerexternal/config/prometheus.yml"),
	})
	require.NoError(t, prom.Setup(context.TODO()))

	return m3, prom, func() {
		assert.NoError(t, prom.Close(context.TODO()))
		assert.NoError(t, m3.Cleanup())
	}
}
