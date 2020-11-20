/*
Copyright (c) 2019 the Octant contributors. All Rights Reserved.
SPDX-License-Identifier: Apache-2.0
*/

package cluster

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_CreateClusterClient(t *testing.T) {
	kubeConfig := filepath.Join("testdata", "kubeconfig.yaml")
	config := RESTConfigOptions{}

	_, err := CreateClusterClient(
		context.TODO(),
		WithKubeConfigList(kubeConfig),
		WithRESTConfigOptions(config),
	)
	require.NoError(t, err)
}
