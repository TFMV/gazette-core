package allocator

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"go.gazette.dev/core/etcdtest"
	"go.gazette.dev/core/keyspace"
	gc "gopkg.in/check.v1"
)

func BenchmarkAll(b *testing.B) {
	b.Run("simulated-deploy", func(b *testing.B) {
		benchmarkSimulatedDeploy(b)
	})
}

type BenchmarkHealthSuite struct{}

// TestBenchmarkHealth runs benchmarks with a small N to ensure they don't bit rot.
func (s *BenchmarkHealthSuite) TestBenchmarkHealth(c *gc.C) {
	var fakeB = testing.B{N: 1}

	benchmarkSimulatedDeploy(&fakeB)
}

var _ = gc.Suite(&BenchmarkHealthSuite{})

func benchmarkSimulatedDeploy(b *testing.B) {
	var client = etcdtest.TestClient()
	defer etcdtest.Cleanup()

	var ctx = context.Background()
	var ks = NewAllocatorKeySpace("/root", testAllocDecoder{})
	var state = NewObservedState(ks, MemberKey(ks, "zone-a", "leader"), isConsistent)

	// Each stage of the deployment will cycle |NMembers10| Members.
	var NMembers10 = b.N
	var NMembersHalf = b.N * 5
	var NMembers = b.N * 10
	var NItems = NMembers * 200

	b.Logf("Benchmarking with %d items, %d members (%d /2, %d /10)", NItems, NMembers, NMembersHalf, NMembers10)

	// fill inserts (if |asInsert|) or modifies keys/values defined by |kvcb| and the range [begin, end).
	var fill = func(begin, end int, asInsert bool, kvcb func(i int) (string, string)) {
		var kv = make([]string, 0, 2*(end-begin))

		for i := begin; i != end; i++ {
			var k, v = kvcb(i)
			kv = append(kv, k)
			kv = append(kv, v)
		}
		if asInsert {
			require.NoError(b, insert(ctx, client, kv...))
		} else {
			require.NoError(b, update(ctx, client, kv...))
		}
	}

	// Insert a Member key which will act as the leader, and will not be rolled.
	require.NoError(b, insert(ctx, client, state.LocalKey, `{"R": 1}`))

	// Announce half of Members...
	fill(0, NMembersHalf, true, func(i int) (string, string) {
		return benchMemberKey(ks, i), `{"R": 1500}`
	})
	// And all Items.
	fill(0, NItems, true, func(i int) (string, string) {
		return ItemKey(ks, fmt.Sprintf("i%05d", i)), `{"R": 3}`
	})

	var testState = struct {
		nextBlock int  // Next block of Members to cycle down & up.
		nextDown  bool // Are we next scaling down, or up?
	}{}

	var testHook = func(round int, idle bool) {
		var begin = NMembers10 * testState.nextBlock
		var end = NMembers10 * (testState.nextBlock + 1)

		log.WithFields(log.Fields{
			"round": round,
			"idle":  idle,
			"begin": begin,
			"end":   end,
		}).Info("ScheduleCallback")

		if !idle {
			return
		} else if err := markAllConsistent(ctx, client, ks, ""); err == nil {
			log.Info("marked some items as consistent")
			return // We marked some items as consistent. Keep going.
		} else if err == io.ErrNoProgress {
			// Continue the next test step below.
		} else {
			log.WithField("err", err).Warn("failed to mark all consistent (will retry)")
			return
		}

		log.WithFields(log.Fields{
			"state.nextBlock": testState.nextBlock,
			"state.nextDown":  testState.nextDown,
		}).Info("next test step")

		if begin == NMembersHalf {
			// We've cycled all Members. Gracefully exit by setting our ItemLimit to zero,
			// and waiting for Serve to complete.
			update(ctx, client, state.LocalKey, `{"R": 0}`)
			return
		}

		if !testState.nextDown {
			// Mark a block of Members as starting up.
			fill(NMembersHalf+begin, NMembersHalf+end, true, func(i int) (string, string) {
				return benchMemberKey(ks, i), `{"R": 1205}` // Less than before, but _just_ enough.
			})
			testState.nextDown = true
		} else {
			// Mark a block of Members as shutting down.
			fill(begin, end, false, func(i int) (string, string) {
				return benchMemberKey(ks, i), `{"R": 0}`
			})
			testState.nextBlock += 1
			testState.nextDown = false
		}
	}

	require.NoError(b, ks.Load(ctx, client, 0))
	go ks.Watch(ctx, client)

	require.NoError(b, Allocate(AllocateArgs{
		Context:  ctx,
		Etcd:     client,
		State:    state,
		TestHook: testHook,
	}))

	log.WithFields(log.Fields{
		"adds":    counterVal(allocatorAssignmentAddedTotal),
		"removes": counterVal(allocatorAssignmentRemovedTotal),
		"packs":   counterVal(allocatorAssignmentPackedTotal),
	}).Info("final metrics")
}

func benchMemberKey(ks *keyspace.KeySpace, i int) string {
	var zone string

	switch i % 5 {
	case 0, 2, 4:
		zone = "zone-a" // Larger than zone-b.
	case 1, 3:
		zone = "zone-b"
	}
	return MemberKey(ks, zone, fmt.Sprintf("m%05d", i))
}

func counterVal(c prometheus.Counter) float64 {
	var out dto.Metric
	if err := c.Write(&out); err != nil {
		panic(err)
	}
	return *out.Counter.Value
}
