package main

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cortexproject/cortex/integration/e2e"
	e2edb "github.com/cortexproject/cortex/integration/e2e/db"
	"github.com/cortexproject/cortex/integration/e2ecortex"
)

const (
	// If you change the image tag, remember to update it in the preloading done
	// by CircleCI too (see .circleci/config.yml).
	previousVersionImage = "quay.io/cortexproject/cortex:v0.6.0"
)

func TestBackwardCompatibilityWithChunksStorage(t *testing.T) {
	s, err := e2e.NewScenario()
	require.NoError(t, err)
	defer s.Close()

	// Start dependencies.
	require.NoError(t, s.StartService(e2edb.NewDynamoDB()))
	require.NoError(t, s.StartService(e2edb.NewConsul()))
	require.NoError(t, s.WaitReady("consul", "dynamodb"))

	// Start Cortex components (ingester running on previous version).
	require.NoError(t, ioutil.WriteFile(
		filepath.Join(s.SharedDir(), cortexSchemaConfigFile),
		[]byte(cortexSchemaConfigYaml),
		os.ModePerm),
	)
	require.NoError(t, s.StartService(e2ecortex.NewTableManager("table-manager", ChunksStorage, "")))
	require.NoError(t, s.StartService(e2ecortex.NewIngester("ingester-1", ChunksStorage, previousVersionImage)))
	require.NoError(t, s.StartService(e2ecortex.NewDistributor("distributor", ChunksStorage, "")))
	require.NoError(t, s.WaitReady("distributor", "ingester-1", "table-manager"))

	// Wait until the first table-manager sync has completed, so that we're
	// sure the tables have been created.
	require.NoError(t, s.Service("table-manager").WaitMetric("cortex_dynamo_sync_tables_seconds", 1))

	// Wait until the distributor has updated the ring.
	require.NoError(t, s.Service("distributor").WaitMetric("cortex_ring_tokens_total", 512))

	// Push some series to Cortex.
	now := time.Now()
	series, expectedVector := generateSeries("series_1", now)

	c, err := e2ecortex.NewClient(s.Endpoint("distributor", 80), "", "user-1")
	require.NoError(t, err)

	res, err := c.Push(series)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)

	// Start ingester-2 on new version, to ensure the transfer is backward compatible.
	require.NoError(t, s.StartService(e2ecortex.NewIngester("ingester-2", mergeFlags(ChunksStorage, map[string]string{
		"-ingester.join-after": "10s",
	}), "")))

	// Stop ingester-1. This function will return once the ingester-1 is successfully
	// stopped, which means the transfer to ingester-2 is completed.
	require.NoError(t, s.StopService("ingester-1"))

	// Query the new ingester both with the old and the new querier.
	for _, image := range []string{previousVersionImage, ""} {
		require.NoError(t, s.StartService(e2ecortex.NewQuerier("querier", ChunksStorage, image)))
		require.NoError(t, s.WaitReady("querier"))

		// Wait until the querier has updated the ring.
		require.NoError(t, s.Service("querier").WaitMetric("cortex_ring_tokens_total", 512))

		// Query the series
		c, err := e2ecortex.NewClient(s.Endpoint("distributor", 80), s.Endpoint("querier", 80), "user-1")
		require.NoError(t, err)

		result, err := c.Query("series_1", now)
		require.NoError(t, err)
		require.Equal(t, model.ValVector, result.Type())
		assert.Equal(t, expectedVector, result.(model.Vector))

		// Stop the querier, so that the test on the next image will work.
		err = s.StopService("querier")
		require.NoError(t, err)
	}
}
