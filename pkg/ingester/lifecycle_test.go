package ingester

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/grafana/dskit/services"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/weaveworks/common/user"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cortexproject/cortex/pkg/chunk"
	"github.com/cortexproject/cortex/pkg/cortexpb"
	"github.com/cortexproject/cortex/pkg/ingester/client"
	"github.com/cortexproject/cortex/pkg/ring"
	"github.com/cortexproject/cortex/pkg/ring/kv/consul"
	"github.com/cortexproject/cortex/pkg/ring/testutils"
	"github.com/cortexproject/cortex/pkg/util/flagext"
	"github.com/cortexproject/cortex/pkg/util/test"
	"github.com/cortexproject/cortex/pkg/util/validation"
)

const userID = "1"

func defaultIngesterTestConfig() Config {
	consul := consul.NewInMemoryClient(ring.GetCodec())
	cfg := Config{}
	flagext.DefaultValues(&cfg)
	flagext.DefaultValues(&cfg.BlocksStorageConfig)
	cfg.FlushCheckPeriod = 99999 * time.Hour
	cfg.MaxChunkIdle = 99999 * time.Hour
	cfg.ConcurrentFlushes = 1
	cfg.LifecyclerConfig.RingConfig.KVStore.Mock = consul
	cfg.LifecyclerConfig.NumTokens = 1
	cfg.LifecyclerConfig.ListenPort = 0
	cfg.LifecyclerConfig.Addr = "localhost"
	cfg.LifecyclerConfig.ID = "localhost"
	cfg.LifecyclerConfig.FinalSleep = 0
	cfg.MaxTransferRetries = 0
	cfg.ActiveSeriesMetricsEnabled = true
	return cfg
}

func defaultClientTestConfig() client.Config {
	clientConfig := client.Config{}
	flagext.DefaultValues(&clientConfig)
	return clientConfig
}

func defaultLimitsTestConfig() validation.Limits {
	limits := validation.Limits{}
	flagext.DefaultValues(&limits)
	return limits
}

// TestIngesterRestart tests a restarting ingester doesn't keep adding more tokens.
func TestIngesterRestart(t *testing.T) {
	config := defaultIngesterTestConfig()
	clientConfig := defaultClientTestConfig()
	limits := defaultLimitsTestConfig()
	config.LifecyclerConfig.UnregisterOnShutdown = false

	{
		_, ingester := newTestStore(t, config, clientConfig, limits, nil)
		time.Sleep(100 * time.Millisecond)
		// Doesn't actually unregister due to UnregisterFromRing: false.
		require.NoError(t, services.StopAndAwaitTerminated(context.Background(), ingester))
	}

	test.Poll(t, 100*time.Millisecond, 1, func() interface{} {
		return testutils.NumTokens(config.LifecyclerConfig.RingConfig.KVStore.Mock, "localhost", ring.IngesterRingKey)
	})

	{
		_, ingester := newTestStore(t, config, clientConfig, limits, nil)
		time.Sleep(100 * time.Millisecond)
		// Doesn't actually unregister due to UnregisterFromRing: false.
		require.NoError(t, services.StopAndAwaitTerminated(context.Background(), ingester))
	}

	time.Sleep(200 * time.Millisecond)

	test.Poll(t, 100*time.Millisecond, 1, func() interface{} {
		return testutils.NumTokens(config.LifecyclerConfig.RingConfig.KVStore.Mock, "localhost", ring.IngesterRingKey)
	})
}

func TestIngester_ShutdownHandler(t *testing.T) {
	for _, unregister := range []bool{false, true} {
		t.Run(fmt.Sprintf("unregister=%t", unregister), func(t *testing.T) {
			config := defaultIngesterTestConfig()
			clientConfig := defaultClientTestConfig()
			limits := defaultLimitsTestConfig()
			config.LifecyclerConfig.UnregisterOnShutdown = unregister
			_, ingester := newTestStore(t, config, clientConfig, limits, nil)

			// Make sure the ingester has been added to the ring.
			test.Poll(t, 100*time.Millisecond, 1, func() interface{} {
				return testutils.NumTokens(config.LifecyclerConfig.RingConfig.KVStore.Mock, "localhost", ring.IngesterRingKey)
			})

			recorder := httptest.NewRecorder()
			ingester.ShutdownHandler(recorder, nil)
			require.Equal(t, http.StatusNoContent, recorder.Result().StatusCode)

			// Make sure the ingester has been removed from the ring even when UnregisterFromRing is false.
			test.Poll(t, 100*time.Millisecond, 0, func() interface{} {
				return testutils.NumTokens(config.LifecyclerConfig.RingConfig.KVStore.Mock, "localhost", ring.IngesterRingKey)
			})
		})
	}
}

func TestIngesterChunksTransfer(t *testing.T) {
	limits, err := validation.NewOverrides(defaultLimitsTestConfig(), nil)
	require.NoError(t, err)

	// Start the first ingester, and get it into ACTIVE state.
	cfg1 := defaultIngesterTestConfig()
	cfg1.LifecyclerConfig.ID = "ingester1"
	cfg1.LifecyclerConfig.Addr = "ingester1"
	cfg1.LifecyclerConfig.JoinAfter = 0 * time.Second
	cfg1.MaxTransferRetries = 10
	ing1, err := New(cfg1, defaultClientTestConfig(), limits, nil, nil, log.NewNopLogger())
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), ing1))

	test.Poll(t, 100*time.Millisecond, ring.ACTIVE, func() interface{} {
		return ing1.lifecycler.GetState()
	})

	// Now write a sample to this ingester
	req, expectedResponse, _, _ := mockWriteRequest(t, labels.Labels{{Name: labels.MetricName, Value: "foo"}}, 456, 123000)
	ctx := user.InjectOrgID(context.Background(), userID)
	_, err = ing1.Push(ctx, req)
	require.NoError(t, err)

	// Start a second ingester, but let it go into PENDING
	cfg2 := defaultIngesterTestConfig()
	cfg2.LifecyclerConfig.RingConfig.KVStore.Mock = cfg1.LifecyclerConfig.RingConfig.KVStore.Mock
	cfg2.LifecyclerConfig.ID = "ingester2"
	cfg2.LifecyclerConfig.Addr = "ingester2"
	cfg2.LifecyclerConfig.JoinAfter = 100 * time.Second
	ing2, err := New(cfg2, defaultClientTestConfig(), limits, nil, nil, log.NewNopLogger())
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), ing2))

	// Let ing2 send chunks to ing1
	ing1.cfg.ingesterClientFactory = func(addr string, _ client.Config) (client.HealthAndIngesterClient, error) {
		return ingesterClientAdapater{
			ingester: ing2,
		}, nil
	}

	// Now stop the first ingester, and wait for the second ingester to become ACTIVE.
	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), ing1))

	test.Poll(t, 10*time.Second, ring.ACTIVE, func() interface{} {
		return ing2.lifecycler.GetState()
	})

	// And check the second ingester has the sample
	matcher, err := labels.NewMatcher(labels.MatchEqual, model.MetricNameLabel, "foo")
	require.NoError(t, err)

	request, err := client.ToQueryRequest(model.TimeFromUnix(0), model.TimeFromUnix(200), []*labels.Matcher{matcher})
	require.NoError(t, err)

	response, err := ing2.Query(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, expectedResponse, response)

	// Check we can send the same sample again to the new ingester and get the same result
	req, _, _, _ = mockWriteRequest(t, labels.Labels{{Name: labels.MetricName, Value: "foo"}}, 456, 123000)
	_, err = ing2.Push(ctx, req)
	require.NoError(t, err)
	response, err = ing2.Query(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, expectedResponse, response)
}

func TestIngesterBadTransfer(t *testing.T) {
	limits, err := validation.NewOverrides(defaultLimitsTestConfig(), nil)
	require.NoError(t, err)

	// Start ingester in PENDING.
	cfg := defaultIngesterTestConfig()
	cfg.LifecyclerConfig.ID = "ingester1"
	cfg.LifecyclerConfig.Addr = "ingester1"
	cfg.LifecyclerConfig.JoinAfter = 100 * time.Second
	ing, err := New(cfg, defaultClientTestConfig(), limits, nil, nil, log.NewNopLogger())
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), ing))

	test.Poll(t, 100*time.Millisecond, ring.PENDING, func() interface{} {
		return ing.lifecycler.GetState()
	})

	// Now transfer 0 series to this ingester, ensure it errors.
	client := ingesterClientAdapater{ingester: ing}
	stream, err := client.TransferChunks(context.Background())
	require.NoError(t, err)
	_, err = stream.CloseAndRecv()
	require.Error(t, err)

	// Check the ingester is still waiting.
	require.Equal(t, ring.PENDING, ing.lifecycler.GetState())
}

type ingesterTransferChunkStreamMock struct {
	ctx  context.Context
	reqs chan *client.TimeSeriesChunk
	resp chan *client.TransferChunksResponse
	err  chan error

	grpc.ServerStream
	grpc.ClientStream
}

func (s *ingesterTransferChunkStreamMock) Send(tsc *client.TimeSeriesChunk) error {
	s.reqs <- tsc
	return nil
}

func (s *ingesterTransferChunkStreamMock) CloseAndRecv() (*client.TransferChunksResponse, error) {
	close(s.reqs)
	select {
	case resp := <-s.resp:
		return resp, nil
	case err := <-s.err:
		return nil, err
	}
}

func (s *ingesterTransferChunkStreamMock) SendAndClose(resp *client.TransferChunksResponse) error {
	s.resp <- resp
	return nil
}

func (s *ingesterTransferChunkStreamMock) ErrorAndClose(err error) {
	s.err <- err
}

func (s *ingesterTransferChunkStreamMock) Recv() (*client.TimeSeriesChunk, error) {
	req, ok := <-s.reqs
	if !ok {
		return nil, io.EOF
	}
	return req, nil
}

func (s *ingesterTransferChunkStreamMock) Context() context.Context {
	return s.ctx
}

func (*ingesterTransferChunkStreamMock) SendMsg(m interface{}) error {
	return nil
}

func (*ingesterTransferChunkStreamMock) RecvMsg(m interface{}) error {
	return nil
}

type ingesterClientAdapater struct {
	client.IngesterClient
	grpc_health_v1.HealthClient
	ingester client.IngesterServer
}

func (i ingesterClientAdapater) TransferChunks(ctx context.Context, _ ...grpc.CallOption) (client.Ingester_TransferChunksClient, error) {
	stream := &ingesterTransferChunkStreamMock{
		ctx:  ctx,
		reqs: make(chan *client.TimeSeriesChunk),
		resp: make(chan *client.TransferChunksResponse),
		err:  make(chan error),
	}
	go func() {
		err := i.ingester.TransferChunks(stream)
		if err != nil {
			stream.ErrorAndClose(err)
		}
	}()
	return stream, nil
}

func (i ingesterClientAdapater) Close() error {
	return nil
}

func (i ingesterClientAdapater) Check(ctx context.Context, in *grpc_health_v1.HealthCheckRequest, opts ...grpc.CallOption) (*grpc_health_v1.HealthCheckResponse, error) {
	return nil, nil
}

// TestIngesterFlush tries to test that the ingester flushes chunks before
// removing itself from the ring.
func TestIngesterFlush(t *testing.T) {
	// Start the ingester, and get it into ACTIVE state.
	store, ing := newDefaultTestStore(t)

	test.Poll(t, 100*time.Millisecond, ring.ACTIVE, func() interface{} {
		return ing.lifecycler.GetState()
	})

	// Now write a sample to this ingester
	var (
		lbls       = []labels.Labels{{{Name: labels.MetricName, Value: "foo"}}}
		sampleData = []cortexpb.Sample{
			{
				TimestampMs: 123000,
				Value:       456,
			},
		}
	)
	ctx := user.InjectOrgID(context.Background(), userID)
	_, err := ing.Push(ctx, cortexpb.ToWriteRequest(lbls, sampleData, nil, cortexpb.API))
	require.NoError(t, err)

	// We add a 100ms sleep into the flush loop, such that we can reliably detect
	// if the ingester is removing its token from Consul before flushing chunks.
	ing.preFlushUserSeries = func() {
		time.Sleep(100 * time.Millisecond)
	}

	// Now stop the ingester.  Don't call shutdown, as it waits for all goroutines
	// to exit.  We just want to check that by the time the token is removed from
	// the ring, the data is in the chunk store.
	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), ing.lifecycler))
	test.Poll(t, 200*time.Millisecond, 0, func() interface{} {
		r, err := ing.lifecycler.KVStore.Get(context.Background(), ring.IngesterRingKey)
		if err != nil {
			return -1
		}
		return len(r.(*ring.Desc).Ingesters)
	})

	// And check the store has the chunk
	res, err := chunk.ChunksToMatrix(context.Background(), store.chunks[userID], model.Time(0), model.Time(math.MaxInt64))
	require.NoError(t, err)
	assert.Equal(t, model.Matrix{
		&model.SampleStream{
			Metric: model.Metric{
				model.MetricNameLabel: "foo",
			},
			Values: []model.SamplePair{
				{Timestamp: model.TimeFromUnix(123), Value: model.SampleValue(456)},
			},
		},
	}, res)
}
